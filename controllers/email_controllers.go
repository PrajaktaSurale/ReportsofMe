package controllers

import (
	"context"
	"email-client/config"
	"email-client/models"
	"email-client/services"
	"fmt"
	"log"
	"mime"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/gin-contrib/sessions"
	"github.com/gin-gonic/gin"
	"go.mongodb.org/mongo-driver/bson"
)

// PreviewOPDHandler renders the OPD template with sample data for preview
func PreviewOPDHandler(c *gin.Context) {
	opd := models.OpdModel{
		PatientName:  "John Doe",
		DoctorName:   "Dr. Smith",
		OPDDate:      "25 March 2025",
		OPDNotes:     "Patient is recovering well. Continue medications.",
		Prescription: "Paracetamol 500mg, Vitamin C",
		FollowupDate: "01 April 2025",
		CreatedOn:    "25 March 2025 03:30 PM",
		GeneratedOn:  "26 March 2025 10:00 AM",
	}
	c.HTML(200, "opdmodal.html", opd)
}
func GeneratePDF(c *gin.Context) {
	var request struct {
		Mobile       string `json:"mobile"`
		DoctorName   string `json:"doctorName"`
		OpdNotes     string `json:"opdNotes"`
		Prescription string `json:"prescription"`
		OpdDate      string `json:"opdDate"`
		FollowupDate string `json:"followupDate"`
		FollowupTime string `json:"followupTime"`
		CreatedOn    string `json:"createdOn"`
	}

	// Parse the incoming JSON body
	if err := c.ShouldBindJSON(&request); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "Invalid request body"})
		return
	}

	// ✅ Get the logged-in email from Gin context (set by AuthMiddleware)
	loggedInEmailValue, exists := c.Get("loggedInEmail")
	if !exists {
		c.JSON(http.StatusUnauthorized, gin.H{"error": "Unauthorized: logged-in email not found"})
		return
	}
	loggedInEmail := loggedInEmailValue.(string)

	// ✅ Get fromName from context
	fromNameValue, exists := c.Get("fromName")
	if !exists {
		c.JSON(http.StatusUnauthorized, gin.H{"error": "Unauthorized: fromName not found"})
		return
	}
	fromName := fromNameValue.(string)

	// Combine follow-up date & time
	followupDateTime := strings.TrimSpace(fmt.Sprintf("%s %s", request.FollowupDate, request.FollowupTime))

	// Construct recipient email
	recipientEmail := fmt.Sprintf("%s@reportsofme.com", request.Mobile)

	// Create OPD data model
	opdData := models.OpdModel{
		PatientName:  strings.TrimSpace(request.Mobile),
		DoctorName:   strings.TrimSpace(request.DoctorName),
		OPDDate:      strings.TrimSpace(request.OpdDate),
		OPDNotes:     strings.TrimSpace(request.OpdNotes),
		Prescription: strings.TrimSpace(request.Prescription),
		FollowupDate: followupDateTime,
		CreatedOn:    time.Now().Format("02 Jan 2006 03:04 PM"),

		GeneratedOn: time.Now().Format("02 Jan 2006 03:04 PM"),
	}

	// Log sending activity
	log.Printf("📧 Generating PDF and sending email from %s (name: %s) to %s", loggedInEmail, fromName, recipientEmail)

	// ✅ Pass fromName to the function
	err := services.GeneratePDFAndSendEmail(opdData, recipientEmail, loggedInEmail, fromName)
	if err != nil {
		log.Println("❌ Error while generating PDF or sending email:", err)
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}

	c.JSON(http.StatusOK, gin.H{"message": "✅ PDF generated and emailed successfully!"})
}

const (
	OTPSubject     = "Your OTP Code"
	SessionUserKey = "user" // Using consistently across controllers
)

var emailRegex = regexp.MustCompile(`^[a-z0-9._%+\-]+@[a-z0-9.\-]+\.[a-z]{2,}$`)
var otpStore = sync.Map{}

// IndexHandler serves the homepage
func IndexHandler(c *gin.Context) {
	c.HTML(http.StatusOK, "index.html", gin.H{"title": "Vault"})
}

// AboutHandler serves the about page
func AboutHandler(c *gin.Context) {
	c.HTML(http.StatusOK, "about.html", gin.H{"title": "Vault"})
}

func DashboardHandler(c *gin.Context) {
	session := sessions.Default(c)

	// Get user email from session
	user := session.Get(SessionUserKey)
	// Get from_name (sender name) from session
	fromName := session.Get("from_name")

	// If no user is found in session, redirect to login
	if user == nil {
		c.Redirect(http.StatusSeeOther, "/login")
		return
	}

	// Log for debugging
	log.Printf("Dashboard - Logged in User: %v, FromName: %v", user, fromName)

	// Render dashboard with email and from_name
	c.HTML(http.StatusOK, "dashboard.html", gin.H{
		"title":    "Vault",
		"Email":    user.(string),
		"FromName": fromName,
	})
}
func GetRecipientsHandler(c *gin.Context) {
	session := sessions.Default(c)
	userEmail := session.Get(SessionUserKey)

	if userEmail == nil {
		log.Println("❌ User not logged in")
		c.JSON(http.StatusUnauthorized, gin.H{"error": "User not logged in"})
		return
	}

	log.Printf("✅ Fetching recipients for user: %s", userEmail)

	// ✅ Pass `userEmail` to `GetUniqueRecipients`
	recipients, err := services.GetUniqueRecipients(userEmail.(string))
	if err != nil {
		log.Println("❌ Error fetching recipients:", err)
		c.JSON(http.StatusInternalServerError, gin.H{"error": "Failed to fetch emails"})
		return
	}

	c.JSON(http.StatusOK, recipients)
}

// LoginHandler handles login, OTP generation, and verification
// LoginHandler handles login, OTP generation, and verification
func LoginHandler(c *gin.Context) {
	var data struct {
		Email        string
		ShowOTP      bool
		ErrorMessage string
	}

	if c.Request.Method == http.MethodPost {
		if err := c.Request.ParseForm(); err != nil {
			log.Printf("Error parsing form: %v", err)
			data.ErrorMessage = "Failed to process form data."
			c.HTML(http.StatusBadRequest, "login.html", data)
			return
		}

		action := c.PostForm("action")
		switch action {

		case "sendotp":
			email := c.PostForm("email")
			if email == "" || !emailRegex.MatchString(email) {
				data.ErrorMessage = "Invalid or empty email."
				c.HTML(http.StatusOK, "login.html", data)
				return
			}

			otpService := services.NewOTPService()
			otp := otpService.GenerateOTP()
			expiration := time.Now().Add(10 * time.Minute)

			otpStore.Store(email, struct {
				OTP        string
				Expiration time.Time
			}{otp, expiration})

			body := fmt.Sprintf("Your OTP is: %s", otp)
			err := services.SendEmail(OTPSubject, body, otp, email)
			if err != nil {
				data.ErrorMessage = fmt.Sprintf("Failed to send OTP: %v", err)
				c.HTML(http.StatusOK, "login.html", data)
				return
			}

			data.Email = email
			data.ShowOTP = true
			c.HTML(http.StatusOK, "login.html", data)

		case "verifyotp":
			email := c.PostForm("email")
			otp := c.PostForm("otp")

			value, ok := otpStore.Load(email)
			if !ok {
				data.ErrorMessage = "Invalid email or OTP session expired."
				data.Email = email
				c.HTML(http.StatusOK, "login.html", data)
				return
			}

			stored := value.(struct {
				OTP        string
				Expiration time.Time
			})

			if time.Now().After(stored.Expiration) {
				otpStore.Delete(email)
				data.ErrorMessage = "OTP has expired. Please request a new one."
				data.Email = email
				c.HTML(http.StatusOK, "login.html", data)
				return
			}

			if otp != stored.OTP {
				data.ErrorMessage = "Invalid OTP. Please try again."
				data.Email = email
				data.ShowOTP = true
				c.HTML(http.StatusOK, "login.html", data)
				return
			}

			// ✅ Fetch fromName using your service function after successful OTP verification
			fromName, err := services.FetchFromNameByEmail(email)
			if err != nil || fromName == "" {
				log.Printf("Could not fetch fromName for %s: %v", email, err)
				fromName = email // fallback if name not found
			}

			otpStore.Delete(email)
			session := sessions.Default(c)
			session.Set(SessionUserKey, email)
			session.Set("from_name", fromName)
			session.Options(sessions.Options{
				MaxAge:   3000, // 50 minutes
				HttpOnly: true,
				Secure:   false,
			})
			if err := session.Save(); err != nil {
				log.Println("Session Save Error:", err)
				data.ErrorMessage = "Failed to create session. Please try again."
				c.HTML(http.StatusOK, "login.html", data)
				return
			}
			log.Printf("Session set for: %s (FromName: %s)", email, fromName)
			c.Redirect(http.StatusSeeOther, "/dashboard")

		default:
			c.JSON(http.StatusBadRequest, gin.H{"message": "Invalid action"})
		}
		return
	}

	c.HTML(http.StatusOK, "login.html", data)
}

// AppointmentListHandler serves the appointment list page
func AppointmentListHandler(c *gin.Context) {
	c.HTML(http.StatusOK, "appointment_list.html", gin.H{"title": "Vault"})
}

func AttachmentHandler(c *gin.Context) {
	filename := c.Param("filename")
	filePath := fmt.Sprintf("./attachments/%s", filename)

	// Check if file exists
	if _, err := os.Stat(filePath); os.IsNotExist(err) {
		c.JSON(http.StatusNotFound, gin.H{"error": "File not found"})
		return
	}

	// Detect MIME type automatically
	mimeType := mime.TypeByExtension(filepath.Ext(filename))
	if mimeType == "" {
		mimeType = "application/octet-stream" // Default fallback
	}

	log.Println("Serving file:", filePath, "with MIME type:", mimeType)

	// Set headers to force opening in a new tab (not downloading)
	c.Header("Content-Type", mimeType)
	c.Header("Content-Disposition", "inline") // Forces browser to display file

	// Serve file
	c.File(filePath)
}

// GetEmailIDs fetches unique recipient email IDs filtered by the logged-in user's email
func GetEmailIDs(c *gin.Context) {
	session := sessions.Default(c)
	userEmail := session.Get(SessionUserKey) // Get the logged-in user's email from session

	if userEmail == nil {
		c.JSON(http.StatusUnauthorized, gin.H{"error": "User not logged in"})
		return
	}

	loggedInEmail := userEmail.(string)
	log.Printf("✅ Fetching recipients for logged-in email: %s", loggedInEmail)

	// Fetch unique recipients
	recipients, err := services.GetUniqueRecipients(loggedInEmail)
	if err != nil {
		log.Printf("❌ Failed to fetch recipients: %v", err)
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}

	// Handle empty results gracefully
	if len(recipients) == 0 {
		log.Println("⚠ No recipients found for this user.")
		c.JSON(http.StatusOK, gin.H{
			"message":    "No recipients found.",
			"recipients": []string{},
		})
		return
	}

	log.Printf("✅ Successfully fetched %d recipients: %v", len(recipients), recipients)

	// Return recipients in JSON
	c.JSON(http.StatusOK, gin.H{
		"recipients": recipients,
	})
}

// EmailHandler handles email fetching and rendering
// EmailHandler processes email-related requests.

func EmailHandler(c *gin.Context) {
	log.Println("🚀 Entering EmailHandler function")

	// ✅ Get logged-in user and fromName from session
	session := sessions.Default(c)
	user := session.Get(SessionUserKey)
	fromName := session.Get("from_name") // retrieved from session

	if user == nil {
		log.Println("❌ No active session found, redirecting to login")
		c.Redirect(http.StatusSeeOther, "/login")
		return
	}

	loggedInEmail, ok := user.(string)
	if !ok || loggedInEmail == "" {
		log.Println("❌ Invalid user session data, redirecting to login")
		c.Redirect(http.StatusSeeOther, "/login")
		return
	}

	// ✅ Fetch unique recipient email IDs using loggedInEmail
	recipientList, err := services.FetchEmailIDs(loggedInEmail)
	if err != nil {
		log.Printf("❌ Error fetching email IDs for %s: %v\n", loggedInEmail, err)
		c.JSON(http.StatusInternalServerError, gin.H{"error": "Failed to fetch email recipients"})
		return
	}

	selectedRecipient := c.Query("to")

	// ✅ Handle AJAX request to fetch emails for the selected recipient
	if c.Request.Header.Get("X-Requested-With") == "XMLHttpRequest" {
		if selectedRecipient == "" {
			log.Println("⚠️ No recipient provided in AJAX request")
			c.JSON(http.StatusBadRequest, gin.H{"error": "Recipient email is required"})
			return
		}

		log.Printf("📩 Fetching emails for recipient: %s\n", selectedRecipient)

		emailList, err := services.FetchEmails(selectedRecipient)
		if err != nil {
			log.Printf("❌ Error fetching emails for %s: %v\n", selectedRecipient, err)
			c.JSON(http.StatusInternalServerError, gin.H{"error": "Failed to fetch emails"})
			return
		}

		if emailList == nil {
			log.Println("⚠️ No emails found for recipient:", selectedRecipient)
			emailList = []models.Message{}
		}

		log.Printf("✅ %d emails fetched for %s\n", len(emailList), selectedRecipient)
		c.JSON(http.StatusOK, gin.H{"emails": emailList})
		return
	}

	// ✅ Render document.html page
	log.Println("📤 Rendering document.html")
	c.HTML(http.StatusOK, "document.html", gin.H{
		"title":            "Vault",
		"Email":            loggedInEmail,
		"UniqueRecipients": recipientList,
		"FromName":         fromName, // coming from session
		"Emails":           []models.Message{},
		"SelectedTo":       selectedRecipient,
	})
}

type EmailResponse struct {
	PlainTextBody string              `json:"plainTextBody"`
	Attachments   []map[string]string `json:"attachments"`
}

func GetPlainTextEmailBody(c *gin.Context) {
	uid := c.DefaultQuery("uid", "")
	if uid == "" {
		c.JSON(http.StatusBadRequest, gin.H{"error": "UID is required in query parameters"})
		return
	}

	// Convert string UID to uint32
	uidInt, err := strconv.ParseUint(uid, 10, 32)
	if err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": fmt.Sprintf("Invalid UID format: %v", err)})
		return
	}

	// Establish IMAP connection
	imapClient, err := config.ConnectIMAP()
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "Failed to connect to IMAP server"})
		return
	}
	defer imapClient.Logout()

	// Fetch plain text body and attachments
	plainTextBody, attachments, err := services.FetchPlainTextEmailBody(imapClient, uint32(uidInt))
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": fmt.Sprintf("Failed to fetch email content: %v", err)})
		return
	}

	// Send response
	emailResponse := EmailResponse{
		PlainTextBody: plainTextBody,
		Attachments:   attachments,
	}
	c.JSON(http.StatusOK, emailResponse)
}

func GetAttachment(c *gin.Context) {
	emailIDStr := c.Query("email_id")
	attachmentName := c.Query("attachment_name")

	// Convert email ID to uint32
	emailID, err := strconv.ParseUint(emailIDStr, 10, 32)
	if err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "Invalid email ID"})
		return
	}

	// Fetch the attachment
	attachmentData, filename, err := services.FetchAttachment(uint32(emailID), attachmentName)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}

	// Return the file as a response
	c.Header("Content-Disposition", "attachment; filename="+filename)
	c.Data(http.StatusOK, "application/octet-stream", attachmentData)
}

func DownloadAttachmentHandler(c *gin.Context) {
	emailIDStr := c.Query("uid") // ✅ This should match URL param `uid`
	attachmentName := c.Query("attachmentName")

	emailID, err := strconv.ParseUint(emailIDStr, 10, 32)
	if err != nil {
		c.Data(http.StatusBadRequest, "text/html", []byte("<h3>Invalid Email ID provided.</h3>"))
		return
	}

	attachmentData, filename, err := services.FetchAttachment(uint32(emailID), attachmentName)
	if err != nil {
		c.Data(http.StatusInternalServerError, "text/html", []byte(fmt.Sprintf("<h3>Attachment not found or failed to load.</h3><p>Error: %v</p>", err)))
		return
	}

	ext := filepath.Ext(filename)
	mimeType := mime.TypeByExtension(ext)
	if mimeType == "" {
		mimeType = http.DetectContentType(attachmentData)
	}

	c.Header("Content-Disposition", fmt.Sprintf("inline; filename=\"%s\"", filename))
	c.Data(http.StatusOK, mimeType, attachmentData)
}

// CheckEmailExistsHandler checks if the email pair exists in MongoDB
func CheckEmailExistsHandler(c *gin.Context) {
	toEmail := c.Query("to")     // PatientId in MongoDB
	fromEmail := c.Query("from") // DoctorId in MongoDB

	// log.Println("🔍 Received From (DoctorId):", fromEmail) // Debug log
	// log.Println("🔍 Received To (PatientId):", toEmail)    // Debug log

	// ✅ Get MongoDB collection for 'RecordAccessRights'
	emailCol := config.GetReportCollection()
	if emailCol == nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "❌ MongoDB not initialized"})
		return
	}

	// ✅ Use the correct field names: "DoctorId" and "PatientId"
	filter := bson.M{"DoctorId": fromEmail, "PatientId": toEmail}

	log.Println("🔍 MongoDB Query Filter:", filter) // Debug log

	// ✅ Check if the record exists
	count, err := emailCol.CountDocuments(context.Background(), filter)
	if err != nil {
		log.Println("❌ MongoDB Query Error:", err)
		c.JSON(http.StatusInternalServerError, gin.H{"error": "Database query failed"})
		return
	}

	if count > 0 {
		log.Println("✅ Email exists in MongoDB")
		c.JSON(http.StatusOK, gin.H{"message": "✅ Email exists in database"})
	} else {
		log.Println("⚠️ Email not found in MongoDB")
		c.JSON(http.StatusOK, gin.H{"message": "⚠️ Email not found in database"})
	}
}

func LogoutHandler(c *gin.Context) {
	session := sessions.Default(c)
	fmt.Println("Before logout:", session.Get("user"))

	session.Clear()
	session.Save()

	fmt.Println("After logout:", session.Get("user")) // Should print `nil`
	c.SetCookie("session_token", "", -1, "/", "", false, true)

	c.Redirect(http.StatusSeeOther, "/login")
}
