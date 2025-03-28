package services

import (
	"bytes"
	"context"
	"crypto/tls"
	"email-client/config"
	"email-client/models"
	"encoding/base64"
	"errors"
	"fmt"
	"html/template"
	"io"
	"io/ioutil"
	"log"
	"math/rand"
	"mime"
	"net/smtp"
	"os"
	"os/exec"
	"path/filepath"

	"strings"

	"time"

	"github.com/emersion/go-imap"
	"github.com/emersion/go-imap/client"
	"github.com/emersion/go-message"
	"github.com/emersion/go-message/mail"
	"github.com/google/uuid"
	"go.mongodb.org/mongo-driver/bson"
	"go.mongodb.org/mongo-driver/mongo"
)

func GeneratePDFAndSendEmail(opdData models.OpdModel, recipientEmail, loggedInEmail, fromName string) error {
	// Step 1: Load Typst template
	templateData, err := os.ReadFile("templates/template.typ")
	if err != nil {
		return fmt.Errorf("failed to read template file: %w", err)
	}

	// Step 2: Skip if follow-up date is missing
	if strings.TrimSpace(opdData.FollowupDate) == "" {
		log.Println("Follow-up date/time is empty — skipping PDF generation")
		return nil
	}

	// Step 3: Replace placeholders in the template
	replacements := map[string]string{
		"#patientname":  opdData.PatientName,
		"#drname":       opdData.DoctorName,
		"#opddate":      opdData.OPDDate,
		"#opdnotes":     opdData.OPDNotes,
		"#prescription": opdData.Prescription,
		"#followupdate": opdData.FollowupDate,
		"#createdon":    opdData.CreatedOn,
	}
	typstContent := string(templateData)
	for key, value := range replacements {
		typstContent = strings.ReplaceAll(typstContent, key, value)
	}

	// Step 4: Write temporary Typst file
	safePatientName := strings.ReplaceAll(opdData.PatientName, " ", "_")
	tempTypFile := fmt.Sprintf("attachments/%s-opd.typ", safePatientName)
	if err := os.WriteFile(tempTypFile, []byte(typstContent), 0644); err != nil {
		return fmt.Errorf("failed to write Typst file: %w", err)
	}
	defer os.Remove(tempTypFile)

	// Step 5: Generate PDF using typst CLI
	pdfFileName := fmt.Sprintf("OPD_%s.pdf", safePatientName)
	pdfFilePath := filepath.Join("attachments", pdfFileName)
	cmd := exec.Command("typst", "compile", tempTypFile, pdfFilePath)
	output, err := cmd.CombinedOutput()
	if err != nil {
		return fmt.Errorf("PDF generation failed: %v\nOutput: %s", err, output)
	}
	log.Println("✅ PDF generated:", pdfFilePath)

	// Step 6: Confirm PDF file exists
	info, err := os.Stat(pdfFilePath)
	if err != nil || info.Size() == 0 {
		return fmt.Errorf("generated PDF file missing or empty at %s", pdfFilePath)
	}
	defer os.Remove(pdfFilePath)

	// Step 7: Read PDF content
	pdfContent, err := os.ReadFile(pdfFilePath)
	if err != nil {
		return fmt.Errorf("failed to read generated PDF: %w", err)
	}

	// Step 8: Send email with PDF attachment (just call the function directly)
	err = SendEmailWithAttachment(
		opdData.PatientName,
		opdData.DoctorName,
		opdData.OPDDate,
		opdData.OPDNotes,
		opdData.Prescription,
		opdData.FollowupDate,
		"", // followupTime (already combined)
		opdData.CreatedOn,
		fmt.Sprintf("OPD  %s", opdData.PatientName),
		recipientEmail,
		pdfFileName,
		pdfContent,
		loggedInEmail,
		fromName,
	)
	if err != nil {
		return fmt.Errorf("failed to send email with PDF: %w", err)
	}

	log.Println("✅ PDF created and email sent to:", recipientEmail, "from display name:", fromName)
	return nil
}

func parseOPDTemplate(templatePath string, data models.OpdModel) (string, error) {
	// Parse the HTML template file
	tmpl, err := template.ParseFiles(templatePath)
	if err != nil {
		return "", fmt.Errorf("failed to parse template file (%s): %w", templatePath, err)
	}

	// Execute the template with provided OPD data
	var tpl bytes.Buffer
	if err := tmpl.Execute(&tpl, data); err != nil {
		return "", fmt.Errorf("failed to execute template: %w", err)
	}

	return tpl.String(), nil
}
func SendEmailWithAttachment(
	patientName, doctorName, opdDate, opdNotes, prescription, followupDate, followupTime, createdOn,
	subject, recipient, filename string, attachment []byte, loggedInEmail, fromName string,
) error {
	// Load SMTP configuration
	smtpConfig, err := config.LoadSMTPConfig()
	if err != nil {
		return fmt.Errorf("failed to load SMTP config: %w", err)
	}

	// SMTP authentication (actual sender)
	auth := smtp.PlainAuth("", smtpConfig.From, smtpConfig.Password, smtpConfig.SMTPHost)
	boundary := "boundary-opd-email-12345"

	// Prepare OPD data for email body
	opdData := models.OpdModel{
		PatientName:  strings.TrimSpace(patientName),
		DoctorName:   strings.TrimSpace(doctorName),
		OPDDate:      strings.TrimSpace(opdDate),
		OPDNotes:     strings.TrimSpace(opdNotes),
		Prescription: strings.TrimSpace(prescription),
		FollowupDate: strings.TrimSpace(fmt.Sprintf("%s %s", followupDate, followupTime)),
		CreatedOn:    strings.TrimSpace(createdOn),
		GeneratedOn:  time.Now().Format("02 Jan 2006 03:04 PM"),
	}

	// Parse HTML body template
	emailBody, err := parseOPDTemplate("templates/opdmodal.html", opdData)
	if err != nil {
		return fmt.Errorf("failed to parse email body template: %v", err)
	}

	// Build email with custom 'From' header showing fromName and loggedInEmail
	var msg bytes.Buffer
	msg.WriteString(fmt.Sprintf("From: \"%s\" <%s>\r\n", fromName, loggedInEmail))
	msg.WriteString(fmt.Sprintf("To: %s\r\n", recipient))
	msg.WriteString(fmt.Sprintf("Subject: %s\r\n", subject))
	msg.WriteString(fmt.Sprintf("Date: %s\r\n", time.Now().Format(time.RFC1123Z)))
	msg.WriteString("MIME-Version: 1.0\r\n")
	msg.WriteString(fmt.Sprintf("Content-Type: multipart/mixed; boundary=%s\r\n", boundary))
	msg.WriteString("\r\n--" + boundary + "\r\n")

	// Add HTML body
	msg.WriteString("Content-Type: text/html; charset=\"utf-8\"\r\n")
	msg.WriteString("Content-Transfer-Encoding: 7bit\r\n\r\n")
	msg.WriteString(emailBody + "\r\n")
	msg.WriteString("\r\n--" + boundary + "\r\n")

	// Add PDF attachment
	msg.WriteString(fmt.Sprintf("Content-Type: application/pdf; name=\"%s\"\r\n", filename))
	msg.WriteString(fmt.Sprintf("Content-Disposition: attachment; filename=\"%s\"\r\n", filename))
	msg.WriteString("Content-Transfer-Encoding: base64\r\n\r\n")

	encodedAttachment := base64.StdEncoding.EncodeToString(attachment)
	for i := 0; i < len(encodedAttachment); i += 76 {
		end := i + 76
		if end > len(encodedAttachment) {
			end = len(encodedAttachment)
		}
		msg.WriteString(encodedAttachment[i:end] + "\r\n")
	}
	msg.WriteString("--" + boundary + "--\r\n")

	// Send mail (authenticated sender is smtpConfig.From)
	err = smtp.SendMail(
		fmt.Sprintf("%s:%s", smtpConfig.SMTPHost, smtpConfig.SMTPPort),
		auth,
		smtpConfig.From, // SMTP sender
		[]string{recipient},
		msg.Bytes(),
	)
	if err != nil {
		return fmt.Errorf("failed to send email: %w", err)
	}

	log.Printf("✅ Email successfully sent to %s (from display: %s <%s>)", recipient, fromName, loggedInEmail)
	return nil
}

// DateService provides current date functionality
type DateService struct{}

func NewDateService() *DateService {
	return &DateService{}
}

func (ds *DateService) GetCurrentDate() time.Time {
	return time.Now()
}

// OTPService handles OTP generation
type OTPService struct{}

func NewOTPService() *OTPService {
	return &OTPService{}
}

func (os *OTPService) GenerateOTP() string {
	rand.Seed(time.Now().UnixNano())
	otp := rand.Intn(1000000)
	formattedOTP := fmt.Sprintf("%06d", otp)
	fmt.Println("Generated OTP (from OTPService):", formattedOTP)
	return formattedOTP
}

// ✅ Struct to store recipient emails and sender names FetchEmailIDs
type EmailRecord struct {
	Email    string `json:"email"`
	FromName string `json:"from_name"`
}

func FetchFromNameByEmail(loggedInEmail string) (string, error) {
	log.Println("🔄 [Service - Name] Starting FetchFromNameByEmail...")

	// Step 1: Connect to IMAP
	imapClient, err := config.ConnectIMAP()
	if err != nil {
		log.Printf("❌ [Service - Name] IMAP connection error: %v", err)
		return "", fmt.Errorf("failed to connect to IMAP: %w", err)
	}
	defer imapClient.Logout()
	log.Println("✅ [Service - Name] IMAP connection successful.")

	// Step 2: Select INBOX
	_, err = imapClient.Select("INBOX", false)
	if err != nil {
		log.Printf("❌ [Service - Name] Failed to select inbox: %v", err)
		return "", fmt.Errorf("failed to select inbox: %v", err)
	}
	log.Println("📂 [Service - Name] INBOX selected.")

	// Step 3: Search for emails where FROM = loggedInEmail
	searchCriteria := imap.NewSearchCriteria()
	searchCriteria.Header.Add("FROM", loggedInEmail)
	log.Printf("🔍 [Service - Name] Searching emails FROM: %s...", loggedInEmail)

	seqNums, err := imapClient.Search(searchCriteria) // Fetch sequence numbers
	if err != nil {
		log.Printf("❌ [Service - Name] Search failed: %v", err)
		return "", fmt.Errorf("search failed: %v", err)
	}
	log.Printf("📊 [Service - Name] Found %d matching emails.", len(seqNums))

	if len(seqNums) == 0 {
		log.Println("⚠️ [Service - Name] No matching emails found. Returning email as fallback.")
		return loggedInEmail, nil
	}

	// Step 4: Process emails from newest to oldest
	log.Println("🔽 [Service - Name] Fetching emails from newest to oldest...")
	for i := len(seqNums) - 1; i >= 0; i-- {
		seqSet := new(imap.SeqSet)
		seqSet.AddNum(seqNums[i]) // Use sequence numbers (not UID)

		messages := make(chan *imap.Message, 1)
		done := make(chan error, 1)

		go func() {
			done <- imapClient.Fetch(seqSet, []imap.FetchItem{imap.FetchEnvelope}, messages)
		}()

		for msg := range messages {
			if msg.Envelope != nil && len(msg.Envelope.From) > 0 {
				for _, from := range msg.Envelope.From {
					emailAddr := from.MailboxName + "@" + from.HostName
					log.Printf("📧 [Service - Name] Checking email: %s", emailAddr)

					if strings.EqualFold(emailAddr, loggedInEmail) && from.PersonalName != "" {
						name := strings.Title(strings.ToLower(from.PersonalName))
						log.Printf("✅ [Service - Name] Found Name: '%s' for Email: %s", name, emailAddr)
						return name, nil
					}
				}
			}
		}

		<-done // Wait for fetch completion
	}

	// Step 5: Fallback if no name found
	log.Println("⚠️ [Service - Name] Name not found, returning fallback email.")
	return loggedInEmail, nil
}

func FetchEmailIDs(loggedInEmail string) ([]string, error) {
	imapClient, err := config.ConnectIMAP()
	if err != nil {
		return nil, fmt.Errorf("failed to connect to IMAP: %w", err)
	}
	defer imapClient.Logout()

	// ✅ Select inbox (readonly mode to prevent accidental changes)
	_, err = imapClient.Select("INBOX", true)
	if err != nil {
		return nil, fmt.Errorf("failed to select INBOX: %w", err)
	}

	// ✅ Search for emails where "FROM" = loggedInEmail (Filter First)
	searchCriteria := imap.NewSearchCriteria()
	searchCriteria.Header.Add("FROM", loggedInEmail) // 🔍 Filter emails by sender

	seqNums, err := imapClient.Search(searchCriteria)
	if err != nil {
		return nil, fmt.Errorf("search failed: %w", err)
	}
	log.Printf("📂 Found %d emails sent by %s", len(seqNums), loggedInEmail)

	if len(seqNums) == 0 {
		return []string{}, nil
	}

	// ✅ Prepare sequence set for fetching
	seqSet := new(imap.SeqSet)
	seqSet.AddNum(seqNums...)

	// ✅ Fetch only the envelope (metadata) of filtered emails
	messages := make(chan *imap.Message, len(seqNums))
	go func() {
		if err := imapClient.Fetch(seqSet, []imap.FetchItem{imap.FetchEnvelope}, messages); err != nil {
			log.Println("❌ Fetch error:", err)
		}
	}()

	uniqueRecipients := make(map[string]bool)

	// ✅ Process fetched emails
	for msg := range messages {
		if msg == nil || msg.Envelope == nil || len(msg.Envelope.To) == 0 {
			continue
		}

		// ✅ Extract recipient emails
		for _, recipient := range msg.Envelope.To {
			toEmail := fmt.Sprintf("%s@%s", recipient.MailboxName, recipient.HostName)
			toEmail = strings.ToLower(strings.TrimSpace(toEmail))

			if toEmail != "" && !uniqueRecipients[toEmail] {
				uniqueRecipients[toEmail] = true
				log.Println("📬 Added recipient:", toEmail)
			}
		}
	}

	// ✅ Convert map to slice
	var recipients []string
	for email := range uniqueRecipients {
		recipients = append(recipients, email)
	}

	log.Printf("✅ Final unique recipients: %v", recipients)
	return recipients, nil
}

// CheckEmailExists checks if an email record (From, To pair) exists in MongoDB
func CheckEmailExists(fromEmail, toEmail string) bool {
	collection := config.GetReportCollection() // Ensure this gets 'RecordAccessRights'

	// Define filter based on the correct field names
	filter := bson.M{"DoctorId": fromEmail, "PatientId": toEmail}

	// Try to find a matching document
	var result bson.M
	err := collection.FindOne(context.TODO(), filter).Decode(&result)

	if err == mongo.ErrNoDocuments {
		// If no document is found, return false
		fmt.Println("❌ No matching record found")
		return false
	} else if err != nil {
		// If any error occurs other than 'no documents', print and return false
		fmt.Println("❌ Error checking email:", err)
		return false
	}

	// If a record is found, print success message and return true
	fmt.Println("✅ Record found in MongoDB")
	return true
}

// FetchEmails retrieves emails filtered by the recipient (To address)
func FetchEmails(toFilter string) ([]models.Message, error) {
	imapClient, err := config.ConnectIMAP()
	if err != nil {
		log.Printf("❌ IMAP Connection Error: %v", err)
		return nil, fmt.Errorf("failed to connect to IMAP: %w", err)
	}
	defer imapClient.Logout()

	// Select INBOX
	mbox, err := imapClient.Select("INBOX", false)
	if err != nil {
		log.Println("❌ Failed to select INBOX:", err)
		return nil, fmt.Errorf("failed to select INBOX: %w", err)
	}

	if mbox.Messages == 0 {
		log.Println("📭 No messages in the inbox.")
		return nil, errors.New("no messages found in mailbox")
	}

	// Search based on 'To' filter if provided
	var seqSet *imap.SeqSet
	if toFilter != "" {
		criteria := imap.NewSearchCriteria()
		criteria.Header.Add("To", toFilter)
		uids, err := imapClient.Search(criteria)
		if err != nil {
			log.Printf("❌ Error during IMAP search: %v", err)
			return nil, fmt.Errorf("error searching emails: %w", err)
		}
		if len(uids) == 0 {
			log.Println("📭 No emails found for given filter.")
			return []models.Message{}, nil
		}
		seqSet = new(imap.SeqSet)
		seqSet.AddNum(uids...)
	} else {
		// Fetch all messages
		from := uint32(1)
		to := mbox.Messages
		seqSet = new(imap.SeqSet)
		seqSet.AddRange(from, to)
	}

	messages := make(chan *imap.Message, 10)
	done := make(chan error, 1)

	// Fetch envelope, body structure & UID
	go func() {
		done <- imapClient.Fetch(seqSet, []imap.FetchItem{
			imap.FetchEnvelope,
			imap.FetchBodyStructure,
			imap.FetchUid,
		}, messages)
	}()

	var emailMessages []models.Message

	for msg := range messages {
		if msg.Envelope == nil || len(msg.Envelope.From) == 0 || len(msg.Envelope.To) == 0 {
			continue
		}

		fromEmail := fmt.Sprintf("%s@%s", msg.Envelope.From[0].MailboxName, msg.Envelope.From[0].HostName)
		toEmail := fmt.Sprintf("%s@%s", msg.Envelope.To[0].MailboxName, msg.Envelope.To[0].HostName)
		fromName := msg.Envelope.From[0].PersonalName

		// Even after IMAP search, do a double-check filter:
		if toFilter != "" && !strings.Contains(strings.ToLower(toEmail), strings.ToLower(toFilter)) {
			continue
		}

		var attachmentNames []string
		if msg.BodyStructure != nil {
			attachmentNames = getAttachmentNames(msg.BodyStructure)
		}

		emailMessages = append(emailMessages, models.Message{
			ID:              fmt.Sprintf("%d", msg.Uid),
			UID:             msg.Uid,
			Subject:         msg.Envelope.Subject,
			From:            fromEmail,
			FromName:        fromName,
			To:              toEmail,
			Date:            msg.Envelope.Date.Format("Jan 02 2006 03:04 PM"),
			AttachmentNames: attachmentNames,
		})
	}

	if err := <-done; err != nil {
		log.Printf("❌ Error while fetching emails: %v", err)
		return nil, fmt.Errorf("error while fetching emails: %w", err)
	}

	return emailMessages, nil
}
func getAttachmentNames(part *imap.BodyStructure) []string {
	var attachments []string

	if part == nil {
		return attachments
	}

	// If part is attachment
	if part.Disposition != "" && strings.ToLower(part.Disposition) == "attachment" {
		filename := part.Params["filename"]
		if filename == "" {
			filename = part.Params["name"]
		}
		if filename != "" {
			attachments = append(attachments, filename)
		}
	}

	// Recursively check sub-parts
	for _, subPart := range part.Parts {
		attachments = append(attachments, getAttachmentNames(subPart)...)
	}

	return attachments
}

type Attachment struct {
	Name string `json:"name"`
	Type string `json:"type"`
	URL  string `json:"url"`
}

func FetchPlainTextEmailBody(imapClient *client.Client, emailUID uint32) (string, []map[string]string, error) {
	_, err := imapClient.Select("INBOX", false)
	if err != nil {
		return "", nil, fmt.Errorf("failed to select mailbox: %w", err)
	}

	seqSet := new(imap.SeqSet)
	seqSet.AddNum(emailUID)
	section := &imap.BodySectionName{}
	messages := make(chan *imap.Message, 1)

	if err := imapClient.UidFetch(seqSet, []imap.FetchItem{section.FetchItem()}, messages); err != nil {
		return "", nil, fmt.Errorf("fetch error: %w", err)
	}

	msg := <-messages
	if msg == nil {
		return "", nil, fmt.Errorf("email not found for UID %d", emailUID)
	}

	body := msg.GetBody(section)
	if body == nil {
		return "", nil, fmt.Errorf("email body empty for UID %d", emailUID)
	}

	rawBody, err := io.ReadAll(body)
	if err != nil {
		return "", nil, fmt.Errorf("error reading body: %w", err)
	}

	// Handle non-MIME fallback
	if !bytes.Contains(rawBody, []byte("Content-Type:")) {
		return string(rawBody), nil, nil
	}

	reader, err := mail.CreateReader(bytes.NewReader(rawBody))
	if err != nil {
		return "", nil, fmt.Errorf("failed to parse MIME email: %w", err)
	}

	var emailBody string
	var plainTextBody string
	var attachments []map[string]string

	for {
		part, err := reader.NextPart()
		if err == io.EOF {
			break
		}
		if err != nil {
			log.Printf("Error reading part: %v", err)
			continue
		}

		contentType, _, _ := mime.ParseMediaType(part.Header.Get("Content-Type"))
		disp, dispParams, _ := mime.ParseMediaType(part.Header.Get("Content-Disposition"))
		filename := dispParams["filename"]
		if filename == "" {
			_, cParams, _ := mime.ParseMediaType(part.Header.Get("Content-Type"))
			filename = cParams["name"]
		}

		log.Printf("Found part: Content-Type=%s, Disposition=%s, Filename=%s", contentType, disp, filename)

		// ✅ Prefer HTML if available
		if strings.HasPrefix(contentType, "text/html") {
			htmlBytes, err := io.ReadAll(part.Body)
			if err == nil {
				emailBody = string(htmlBytes)
			}
		} else if strings.HasPrefix(contentType, "text/plain") {
			textBytes, err := io.ReadAll(part.Body)
			if err == nil {
				plainTextBody = string(textBytes)
			}
		} else if (strings.EqualFold(disp, "attachment") || strings.EqualFold(disp, "inline")) && strings.Contains(contentType, "image") {
			data, err := io.ReadAll(part.Body)
			if err != nil {
				continue
			}
			base64Img := base64.StdEncoding.EncodeToString(data)
			attachments = append(attachments, map[string]string{
				"type":   "image",
				"name":   filename,
				"base64": fmt.Sprintf("data:%s;base64,%s", contentType, base64Img),
			})
		}
	}

	// ✅ If HTML not found, fallback to plain text
	if emailBody == "" && plainTextBody != "" {
		emailBody = plainTextBody
	}

	if emailBody == "" {
		return "", attachments, fmt.Errorf("no email body found for UID %d", emailUID)
	}

	return emailBody, attachments, nil
}

//	func encodeToBase64(data []byte) string {
//		return base64.StdEncoding.EncodeToString(data)
//	}
func FetchAttachment(emailUID uint32, attachmentName string) ([]byte, string, error) {
	imapClient, err := config.ConnectIMAP()
	if err != nil {
		return nil, "", fmt.Errorf("IMAP connection error: %v", err)
	}
	defer imapClient.Logout()

	_, err = imapClient.Select("INBOX", false)
	if err != nil {
		return nil, "", fmt.Errorf("failed to select INBOX: %v", err)
	}

	seqSet := new(imap.SeqSet)
	seqSet.AddNum(emailUID)

	section := &imap.BodySectionName{}
	messages := make(chan *imap.Message, 1)
	errChan := make(chan error, 1)

	fmt.Printf("📥 Fetching UID: %d, looking for attachment: %s\n", emailUID, attachmentName)

	go func() {
		errChan <- imapClient.UidFetch(seqSet, []imap.FetchItem{section.FetchItem()}, messages)
	}()

	msg := <-messages
	fetchErr := <-errChan
	if fetchErr != nil {
		return nil, "", fmt.Errorf("fetch error: %v", fetchErr)
	}

	if msg == nil {
		return nil, "", fmt.Errorf("no message found with UID %d", emailUID)
	}

	r := msg.GetBody(section)
	if r == nil {
		return nil, "", fmt.Errorf("failed to get body for UID %d", emailUID)
	}

	m, err := message.Read(r)
	if err != nil {
		return nil, "", fmt.Errorf("failed to parse email: %v", err)
	}

	mr := m.MultipartReader()
	if mr == nil {
		return nil, "", fmt.Errorf("email is not multipart; no attachments found")
	}

	for {
		p, err := mr.NextPart()
		if err == io.EOF {
			break
		}
		if err != nil {
			return nil, "", fmt.Errorf("error reading multipart part: %v", err)
		}

		disposition, params, _ := p.Header.ContentDisposition()
		_, cParams, _ := p.Header.ContentType()
		filename := params["filename"]
		if filename == "" {
			filename = cParams["name"]
		}

		if (strings.EqualFold(disposition, "attachment") || strings.EqualFold(disposition, "inline")) &&
			strings.EqualFold(filename, attachmentName) {
			data, err := io.ReadAll(p.Body)
			if err != nil {
				return nil, "", fmt.Errorf("error reading attachment body: %v", err)
			}
			return data, filename, nil
		}
	}

	return nil, "", fmt.Errorf("attachment %s not found in email UID %d", attachmentName, emailUID)
}

func SendEmail(subject, body, otp, recipient string) error {
	// Load SMTP config from config.go
	smtpConfig, err := config.LoadSMTPConfig()
	if err != nil {
		return fmt.Errorf("failed to load SMTP configuration: %v", err)
	}

	// Construct the email message
	message := fmt.Sprintf("Subject: %s\r\n\r\n%s\r\n\r\nYour OTP code is: %s", subject, body, otp)

	auth := smtp.PlainAuth("", smtpConfig.From, smtpConfig.Password, smtpConfig.SMTPHost)

	if smtpConfig.SMTPSecurity == "false" {
		// No TLS mode
		log.Println("Warning: Sending email without TLS")
		err = smtp.SendMail(smtpConfig.SMTPHost+":"+smtpConfig.SMTPPort, auth, smtpConfig.From, []string{recipient}, []byte(message))
		if err != nil {
			return fmt.Errorf("failed to send email: %v", err)
		}
	} else {
		// Secure mode
		log.Println("Using TLS for secure email transmission")
		tlsConfig := &tls.Config{
			InsecureSkipVerify: true, // only for testing
			ServerName:         smtpConfig.SMTPHost,
		}

		conn, err := tls.Dial("tcp", smtpConfig.SMTPHost+":"+smtpConfig.SMTPPort, tlsConfig)
		if err != nil {
			return fmt.Errorf("failed to connect over TLS: %v", err)
		}
		defer conn.Close()

		client, err := smtp.NewClient(conn, smtpConfig.SMTPHost)
		if err != nil {
			return fmt.Errorf("failed to create SMTP client: %v", err)
		}
		defer client.Close()

		if err = client.Auth(auth); err != nil {
			return fmt.Errorf("authentication failed: %v", err)
		}

		if err = client.Mail(smtpConfig.From); err != nil {
			return fmt.Errorf("failed to set sender: %v", err)
		}
		if err = client.Rcpt(recipient); err != nil {
			return fmt.Errorf("failed to set recipient: %v", err)
		}

		wc, err := client.Data()
		if err != nil {
			return fmt.Errorf("failed to open data connection: %v", err)
		}
		_, err = wc.Write([]byte(message))
		if err != nil {
			return fmt.Errorf("failed to write message: %v", err)
		}
		wc.Close()
		client.Quit()
	}

	log.Println("✅ Email sent to:", recipient)
	return nil
}
func GetUniqueRecipients(loggedInEmail string) ([]string, error) {
	log.Printf("📩 Fetching unique recipients for logged-in email: %s", loggedInEmail)

	// ✅ Fetch recipients using logged-in email
	emails, err := FetchEmailIDs(loggedInEmail)
	if err != nil {
		return nil, fmt.Errorf("❌ Failed to fetch emails: %w", err)
	}

	// ✅ Use a map to track unique recipients (avoid duplicates)
	emailSet := make(map[string]struct{})
	normalizedSender := strings.ToLower(strings.TrimSpace(loggedInEmail))

	for _, recipient := range emails {
		normalizedEmail := strings.ToLower(strings.TrimSpace(recipient))

		// Ensure it's a valid email and not the sender itself
		if normalizedEmail != "" && normalizedEmail != normalizedSender {
			if _, exists := emailSet[normalizedEmail]; !exists {
				log.Printf("📨 Adding recipient: %s", normalizedEmail)
				emailSet[normalizedEmail] = struct{}{}
			}
		}
	}

	// ✅ Convert map keys to a slice
	uniqueEmails := make([]string, 0, len(emailSet))
	for email := range emailSet {
		uniqueEmails = append(uniqueEmails, email)
	}

	// ✅ Log results
	if len(uniqueEmails) == 0 {
		log.Println("📭 No unique recipients found.")
	} else {
		log.Printf("✅ %d Unique recipients found: %v", len(uniqueEmails), uniqueEmails)
	}

	return uniqueEmails, nil
}

// GeneratePDFFromSelectedValue takes the patient (mobile or email), generates a Typst file, compiles to PDF, and returns the path-GeneratePDFService
func GeneratePDFFromSelectedValue(selectedValue string) (string, error) {
	// Load the Typst template
	templateContent, err := ioutil.ReadFile("template-html/template.typ")
	if err != nil {
		return "", fmt.Errorf("error reading template: %v", err)
	}

	// Replace #content with the selected patient value
	finalTypst := strings.Replace(string(templateContent), "#content", selectedValue, 1)

	// Unique filename for each generation
	uniqueID := uuid.New().String()
	typstFile := fmt.Sprintf("output-%s.typ", uniqueID)
	outputPDF := fmt.Sprintf("output-%s.pdf", uniqueID)

	// Write the Typst input file
	err = ioutil.WriteFile(typstFile, []byte(finalTypst), 0644)
	if err != nil {
		return "", fmt.Errorf("error writing typst file: %v", err)
	}

	// Run typst compile command
	cmd := exec.Command("typst", "compile", typstFile, outputPDF)
	err = cmd.Run()
	if err != nil {
		return "", fmt.Errorf("typst compile failed: %v", err)
	}

	// Optional: clean up .typ file after some delay
	go func() {
		time.Sleep(10 * time.Second)
		_ = os.Remove(typstFile)
	}()

	return outputPDF, nil
}
