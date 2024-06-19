package post

import (
	"context"
	"fmt"
	"log/slog"
	"net/http"
	"strings"

	"github.com/GoogleCloudPlatform/functions-framework-go/functions"
	"github.com/go-playground/validator/v10"
	"google.golang.org/api/drive/v3"
	"google.golang.org/api/sheets/v4"
)

var validate *validator.Validate
var driveService *drive.Service
var sheetsService *sheets.Service

const MAX_REQUEST_SIZE = 20 << 20 // 20 MB
const MAX_UPLOAD_SIZE = 15 << 20  // 15 MB

func init() {
	createGoogleSheetsService(context.TODO())

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
		Email     string `json:"email" validate:"required,email"`
		Mobile    string `json:"mobile" validate:"e164"`
		FirstName string `json:"firstName" validate:"required"`
		LastName  string `json:"lastName" validate:"required"`
		Enquiry   string `json:"enquiry" validate:"required"`
	}

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

	enquiriesSheetName := ""     // ENV
	enquiriesSpreadsheetId := "" // ENV
	res, err := sheetsService.Spreadsheets.Values.
		Append(enquiriesSpreadsheetId, enquiriesSheetName, &sheets.ValueRange{
			Values: [][]interface{}{
				{
					body.FirstName, body.LastName,
					body.Enquiry, body.Email,
					body.Mobile,
				},
			},
		}).
		InsertDataOption("INSERT_ROWS").
		Context(r.Context()).
		Fields("spreadsheetId, tableRange, updates").
		Do()
	if err != nil {
		slog.ErrorContext(r.Context(), "error", "gsheets", err.Error())
	}
	if res.HTTPStatusCode < 200 || res.HTTPStatusCode >= 300 {
		slog.ErrorContext(r.Context(), "error", "gsheets", fmt.Sprintf("%+v", res))
	}

	constactsSpreadsheetId := ""
	sheetsService.Spreadsheets.BatchUpdate(constactsSpreadsheetId, &sheets.BatchUpdateSpreadsheetRequest{
		Requests: []*sheets.Request{
			{
				AppendCells: &sheets.AppendCellsRequest{
					Fields: "*",
					Rows:   []*sheets.RowData{}, // TODO
				},
				DeleteDuplicates: &sheets.DeleteDuplicatesRequest{
					ComparisonColumns: []*sheets.DimensionRange{}, // TODO
					Range:             &sheets.GridRange{},        // TODO
				},
			},
		},
	})

	slog.Info("appended", "gsheets", fmt.Sprintf("%v+", res))

	// TODO: Send email

	slog.Info("processed", "enquiry", fmt.Sprintf("%+v", body))
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

func createGoogleSheetsService(ctx context.Context) *sheets.Service {
	service, err := sheets.NewService(ctx)
	if err != nil {
		slog.Error("error", "gsheets service", err.Error())

		panic(err)
	}

	return service
}
