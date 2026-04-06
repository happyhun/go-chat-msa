package user

import (
	"errors"
	"fmt"
	"regexp"
	"unicode/utf8"

	"go-chat-msa/internal/shared/config"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgtype"
)

var (
	usernameRegex        = regexp.MustCompile(`^[a-zA-Z0-9\x{AC00}-\x{D7A3}]+$`)
	allowedPasswordChars = regexp.MustCompile(`^[a-zA-Z0-9!\"#$%&'()*+,\-./:;<=>?@\[\\\]^_\x60{|}~]+$`)
	passwordPatterns     = []*regexp.Regexp{
		regexp.MustCompile(`[A-Z]`),
		regexp.MustCompile(`[a-z]`),
		regexp.MustCompile(`[0-9]`),
		regexp.MustCompile(`[!\"#$%&'()*+,\-./:;<=>?@\[\\\]^_\x60{|}~]`),
	}
)

func validateUsername(username string, cfg config.UserConfig) error {
	length := utf8.RuneCountInString(username)
	if length < cfg.Validation.MinUsernameLength || length > cfg.Validation.MaxUsernameLength {
		return fmt.Errorf("nickname must be %d~%d characters", cfg.Validation.MinUsernameLength, cfg.Validation.MaxUsernameLength)
	}

	if !usernameRegex.MatchString(username) {
		return errors.New("nickname can only contain alphanumeric characters and Hangul")
	}
	return nil
}

func validateRoomName(name string, maxLength int) error {
	length := utf8.RuneCountInString(name)
	if length == 0 {
		return errors.New("room name is required")
	}
	if length > maxLength {
		return fmt.Errorf("room name must be at most %d characters", maxLength)
	}
	return nil
}

func validateCapacity(capacity, maxCapacity int32) error {
	if capacity <= 0 {
		return fmt.Errorf("capacity must be positive")
	}
	if capacity > maxCapacity {
		return fmt.Errorf("capacity exceeds maximum allowed (%d)", maxCapacity)
	}
	return nil
}

func validatePassword(password string, cfg config.UserConfig) error {
	if l := len(password); l < cfg.Validation.MinPasswordLength || l > cfg.Validation.MaxPasswordLength {
		return fmt.Errorf("password must be %d~%d characters", cfg.Validation.MinPasswordLength, cfg.Validation.MaxPasswordLength)
	}

	if !allowedPasswordChars.MatchString(password) {
		return errors.New("password contains invalid characters (only alphanumeric and standard special characters are allowed)")
	}

	matchedTypes := 0
	for _, p := range passwordPatterns {
		if p.MatchString(password) {
			matchedTypes++
		}
	}

	if matchedTypes < 3 {
		return errors.New("password must include at least 3 types of: uppercase, lowercase, numbers, and special characters")
	}
	return nil
}

func toPGUUID(id string) (pgtype.UUID, error) {
	parsed, err := uuid.Parse(id)
	if err != nil {
		return pgtype.UUID{}, err
	}
	return pgtype.UUID{Bytes: parsed, Valid: true}, nil
}
