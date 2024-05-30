package post

import (
	"context"
	"fmt"
	"log/slog"
	"mime/multipart"
	"net/http"
	"strings"
	"sync"

	"github.com/GoogleCloudPlatform/functions-framework-go/functions"
	"github.com/go-playground/validator/v10"
	"github.com/google/uuid"
	"google.golang.org/api/drive/v3"
)

var validate *validator.Validate
var driveService *drive.Service

const MAX_REQUEST_SIZE = 20 << 20 // 20 MB
const MAX_UPLOAD_SIZE = 15 << 20  // 15 MB

func init() {
	validate = validator.New(validator.WithRequiredStructEnabled())

	functions.HTTP("Handler", Handler)
}

func Handler(w http.ResponseWriter, r *http.Request) {
	slog.Info(r.Method, "host", r.Host, "path", r.URL.Path)

	if r.Method != "POST" {
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}

	r.Body = http.MaxBytesReader(w, r.Body, MAX_REQUEST_SIZE)
	if err := r.ParseMultipartForm(MAX_UPLOAD_SIZE); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)

		return
	}
	defer r.MultipartForm.RemoveAll()

	var body struct {
		uuid      string
		Email     string `json:"email" validate:"required,email"`
		Mobile    string `json:"mobile" validate:"e164"`
		FirstName string `json:"firstName" validate:"required"`
		LastName  string `json:"lastName" validate:"required"`
		Enquiry   string `json:"enquiry" validate:"required"`
	}

	body.uuid = uuid.NewString()
	body.Email = r.FormValue("email")
	body.Mobile = r.FormValue("mobile")
	body.FirstName = r.FormValue("firstName")
	body.LastName = r.FormValue("lastName")
	body.Enquiry = r.FormValue("enquiry")

	slog.Info("begin", "enquiry", fmt.Sprintf("%+v", body))

	err := validate.Struct(body)
	if err != nil {
		validationErrs := err.(validator.ValidationErrors)

		errs := []string{}
		for i := range validationErrs {
			err := validationErrs[i]

			errs = append(errs, fmt.Sprintf("- %s", err.Error()))
		}

		slog.Error("error", "enquiry", body)
		http.Error(w, fmt.Sprintf("Invalid field values:\n%s", strings.Join(errs, "\n")), http.StatusBadRequest)

		return
	}

	files := r.MultipartForm.File["files"]

	if len(files) > 0 {
		if driveService == nil {
			driveService = createGoogleDriveService()
		}

		about, err := driveService.About.
			Get().
			Fields("storageQuota").
			Context(r.Context()).
			Do()
		if err != nil {
			slog.Error("error", "gdrive about", err.Error())
			http.Error(w, err.Error(), http.StatusInternalServerError)

			return
		}

		slog.Info("stats", "gdrive usage", about.StorageQuota.UsageInDrive, "gdrive limit", about.StorageQuota.Limit)

		uploadedFiles := make(chan drive.File)
		failedToUpload := make(chan int)

		uploadCtx, cancel := context.WithCancel(r.Context())

		var fileUploadWg sync.WaitGroup
		uploadFile := func(fileHeader *multipart.FileHeader, idx int, wg *sync.WaitGroup) {
			defer wg.Done()

			select {
			case <-uploadCtx.Done():
				return
			default:
			}

			slog.Info("begin", "upload", fileHeader.Filename, "size", fileHeader.Size)

			file, err := fileHeader.Open()
			if err != nil {

				failedToUpload <- idx
				slog.Error("error", "open file", fileHeader.Filename, "email", body.Email)

				cancel()
				return
			}
			defer file.Close()

			res, err := driveService.Files.
				Create(&drive.File{
					Name: fileHeader.Filename,
					Properties: map[string]string{
						"lead":      body.uuid,
						"email":     body.Email,
						"firstName": body.FirstName,
						"lastName":  body.LastName,
						"mobile":    body.Mobile,
					},
				}).
				Media(file).
				Fields("id, webContentLink").
				Context(uploadCtx).
				Do()

			if err != nil {
				failedToUpload <- idx
				slog.Error("error", "upload", err.Error(), "email", body.Email)

				cancel()
				return
			}

			slog.Info("end", "upload", fileHeader.Filename, "link", res.WebContentLink)

			uploadedFiles <- *res
		}
		for idx, fileHeader := range files {
			fileUploadWg.Add(1)
			go uploadFile(fileHeader, idx, &fileUploadWg)
		}

		go func() {
			fileUploadWg.Wait()
			close(uploadedFiles)
			close(failedToUpload)
		}()

		select {
		case <-uploadCtx.Done():
			for file := range uploadedFiles {
				driveService.Files.Delete(file.Id).Do()
			}

			http.Error(w, "Failed to upload", http.StatusInternalServerError)

			return
		default:
		}

		attachedFiles := []string{}
		for file := range uploadedFiles {
			attachedFiles = append(attachedFiles, fmt.Sprintf("- %s", file.WebContentLink))
		}
		enquiryWithFiles := fmt.Appendf([]byte(body.Enquiry), "\nAttached files:\n%s", strings.Join(attachedFiles, "\n"))
		body.Enquiry = string(enquiryWithFiles)
	}

	slog.Info("processed", "enquiry", fmt.Sprintf("%+v", body))

	// TODO: POST to CRM
	// TODO: Send email
}

func createGoogleDriveService() *drive.Service {
	// Authenticate using client default credentials
	// see: https://cloud.google.com/docs/authentication/client-libraries
	// Note: Service Account Token Creator IAM role must be granted to the service account
	ctx := context.Background()
	service, err := drive.NewService(ctx)
	if err != nil {
		slog.Error("error", "gdrive service", err.Error())

		panic(err)
	}

	return service
}
