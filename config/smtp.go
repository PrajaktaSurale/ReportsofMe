package config

import (
	"log"
	"os"

	"github.com/joho/godotenv"
)

// SMTPConfig holds SMTP configuration
type SMTPConfig struct {
	From         string
	Password     string
	SMTPHost     string
	SMTPPort     string
	SMTPSecurity string
}

// LoadSMTPConfig loads SMTP environment variables
func LoadSMTPConfig() (*SMTPConfig, error) {
	// Load env
	err := godotenv.Load()
	if err != nil {
		log.Println("Warning: Could not load .env file, using system environment variables")
	}

	config := &SMTPConfig{
		From:         os.Getenv("SMTP_EMAIL"),
		Password:     os.Getenv("SMTP_PASSWORD"),
		SMTPHost:     os.Getenv("SMTP_HOST"),
		SMTPPort:     os.Getenv("SMTP_PORT"),
		SMTPSecurity: os.Getenv("SMTP_SECURITY"),
	}

	// Validation
	if config.From == "" || config.Password == "" || config.SMTPHost == "" || config.SMTPPort == "" {
		return nil, err
	}

	return config, nil
}
