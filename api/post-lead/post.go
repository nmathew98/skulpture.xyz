package post

import (
	"context"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"strings"

	"github.com/GoogleCloudPlatform/functions-framework-go/functions"
	"github.com/go-playground/validator/v10"
	"github.com/google/uuid"
	"google.golang.org/api/drive/v3"
	"google.golang.org/api/option"
)

var validate *validator.Validate
var driveService *drive.Service

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

	// Limit max request size to 20 MB
	const MAX_REQUEST_SIZE = 20 << 20
	r.Body = http.MaxBytesReader(w, r.Body, MAX_REQUEST_SIZE)

	const MAX_UPLOAD_SIZE = 15 << 20 // 15 MB
	if err := r.ParseMultipartForm(MAX_UPLOAD_SIZE); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)

		return
	}
	defer r.MultipartForm.RemoveAll()

	body := struct {
		uuid      string
		Email     string `json:"email" validate:"required,email"`
		Mobile    string `json:"mobile" validate:"e164"`
		FirstName string `json:"firstName" validate:"required"`
		LastName  string `json:"lastName" validate:"required"`
		Enquiry   string `json:"enquiry" validate:"required"`
	}{}

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

		slog.Error("error", "request", body)
		http.Error(w, fmt.Sprintf("Invalid field values:\n%s", strings.Join(errs, "\n")), http.StatusBadRequest)

		return
	}

	uploadedFileLinks := []string{}
	uploadedFiles := []*drive.File{}
	files := r.MultipartForm.File["file"]

	if driveService == nil {
		driveService = createGoogleDriveService()
	}

	about, err := driveService.About.Get().Do()
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)

		return
	}
	limit := about.StorageQuota.Limit
	currentUsage := about.StorageQuota.UsageInDrive

	for _, fileHeader := range files {
		slog.Info("begin", "upload", fileHeader.Filename, "size", fileHeader.Size)

		currentUsage += fileHeader.Size
		slog.Info("stats", "gdrive usage", currentUsage, "gdrive limit", limit)

		if currentUsage == limit {
			slog.Error("gdrive usage exceeds limit")

			break
		}

		file, err := fileHeader.Open()
		if err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)

			return
		}
		defer file.Close()

		res, err := driveService.Files.
			Create(&drive.File{
				Name: fileHeader.Filename,
				Properties: map[string]string{
					"leadId":    body.uuid,
					"email":     body.Email,
					"firstName": body.FirstName,
					"lastName":  body.LastName,
					"mobile":    body.Mobile,
				},
			}).
			Media(file).
			Do()

		if err != nil {
			slog.Error("error", "upload", err.Error(), "email", body.Email)

			http.Error(w, err.Error(), http.StatusInternalServerError)

			return
		}

		slog.Info("end", "upload", fileHeader.Filename, "link", res.WebContentLink)

		uploadedFiles = append(uploadedFiles, res)
		uploadedFileLinks = append(uploadedFileLinks, fmt.Sprintf("- %s", res.WebContentLink))
	}

	if currentUsage == limit {
		for _, file := range uploadedFiles {
			driveService.Files.Delete(file.Id)
		}

		http.Error(w, "Google Drive quota reached", http.StatusInsufficientStorage)

		return
	}

	if len(uploadedFileLinks) > 0 {
		enquiryWithFiles := fmt.Appendf([]byte(body.Enquiry), "\nAttached files:\n%s", strings.Join(uploadedFileLinks, "\n"))

		body.Enquiry = string(enquiryWithFiles)
	}

	slog.Info("processed", "enquiry", fmt.Sprintf("%+v", body))

	// TODO: POST to CRM
	// TODO: Send email
}

func createGoogleDriveService() *drive.Service {
	if os.Getenv("GO_ENV") != "Production" {
		service, err := drive.NewService(context.Background(), option.WithCredentialsFile(os.Getenv("GCLOUD_CREDENTIALS")))
		if err != nil {
			slog.Error("error", "gdrive dev", err.Error())

			panic(err)
		}

		return service
	}

	// Authenticate using client default credentials
	// see: https://cloud.google.com/docs/authentication/client-libraries
	ctx := context.Background()
	service, err := drive.NewService(ctx)
	if err != nil {
		slog.Error("error", "gdrive prod", err.Error())

		panic(err)
	}

	return service
}
