package post

import (
	"context"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"strconv"
	"strings"

	"github.com/GoogleCloudPlatform/functions-framework-go/functions"
	"github.com/go-playground/validator/v10"
	"github.com/google/uuid"
	"github.com/mrz1836/postmark"
	"google.golang.org/api/drive/v3"
)

var validate *validator.Validate
var driveService *drive.Service
var postmarkClient *postmark.Client

const MAX_REQUEST_SIZE = 20 << 20 // 20 MB
const MAX_UPLOAD_SIZE = 15 << 20  // 15 MB

const POSTMARK_TEMPLATE_ENV = "POSTMARK_TEMPLATE"
const POSTMARK_FROM_ENV = "POSTMARK_FROM_ENV"
const SERVER_TOKEN_ENV = "POSTMARK_SERVER"
const ACCOUNT_TOKEN_ENV = "POSTMARK_ACCOUNT"

func init() {
	validate = validator.New(validator.WithRequiredStructEnabled())

	functions.HTTP("Handler", Handler)
}

func Handler(w http.ResponseWriter, r *http.Request) {
	if postmarkClient == nil {
		postmarkClient = createPostmarkClient()
	}

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

	uploadedFileLinks := []string{}
	uploadedFiles := []*drive.File{}
	files := r.MultipartForm.File["files"]

	if len(files) > 0 {
		if driveService == nil {
			driveService = createGoogleDriveService()
		}

		about, err := driveService.About.Get().Fields("storageQuota").Do()
		if err != nil {
			slog.Error("error", "gdrive about", err.Error())
			http.Error(w, err.Error(), http.StatusInternalServerError)

			return
		}
		limit := about.StorageQuota.Limit
		currentUsage := about.StorageQuota.UsageInDrive

		slog.Info("stats", "gdrive usage", currentUsage, "gdrive limit", limit)

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
						"lead":      body.uuid,
						"email":     body.Email,
						"firstName": body.FirstName,
						"lastName":  body.LastName,
						"mobile":    body.Mobile,
					},
				}).
				Media(file).
				Fields("id, webContentLink").
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
	}

	slog.Info("processed", "enquiry", fmt.Sprintf("%+v", body))

	// TODO: POST to CRM
	// TODO: Send email
	templateId, err := strconv.ParseInt(os.Getenv(POSTMARK_TEMPLATE_ENV), 10, 64)
	if err != nil {
		slog.Error("error", "env unspecified", POSTMARK_TEMPLATE_ENV)

		panic(fmt.Errorf("environment variable %s must be specified", POSTMARK_TEMPLATE_ENV))
	}

	postmarkFrom := os.Getenv(POSTMARK_FROM_ENV)
	if postmarkFrom == "" {
		slog.Error("error", "env unspecified", POSTMARK_FROM_ENV)

		panic(fmt.Errorf("environment variable %s must be specified", POSTMARK_FROM_ENV))
	}

	res, err := postmarkClient.SendTemplatedEmail(context.Background(), postmark.TemplatedEmail{
		TemplateID:    int64(templateId),
		From:          postmarkFrom,
		To:            body.Email,
		TrackOpens:    true,
		TemplateModel: map[string]interface{}{}, // TODO: Template model
	})
	if err != nil {
		slog.Error("error", "postmark", err.Error())
	}

	slog.Info("sent", "postmark message id", res.MessageID, "to", res.To, "at", res.SubmittedAt, "lead", body.uuid)
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

func createPostmarkClient() *postmark.Client {
	if os.Getenv(SERVER_TOKEN_ENV) == "" || os.Getenv(ACCOUNT_TOKEN_ENV) == "" {
		slog.Error("error", "unspecified", fmt.Sprintf("%s, %s", SERVER_TOKEN_ENV, ACCOUNT_TOKEN_ENV))

		panic(fmt.Errorf("environment variables `%s` and `%s` must be specified", SERVER_TOKEN_ENV, ACCOUNT_TOKEN_ENV))
	}

	client := postmark.NewClient(os.Getenv("POSTMARK_SERVER"), os.Getenv("POSTMARK_ACCOUNT"))

	return client
}
