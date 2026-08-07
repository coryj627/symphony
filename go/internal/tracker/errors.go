package tracker

import "errors"

var ErrInvalidTrackerConfig = errors.New("invalid_tracker_config")

// ConfigError identifies a configuration control that the UI can safely show
// alongside a validation message.
type ConfigError struct {
	Field  string
	Detail string
}

func (err *ConfigError) Error() string {
	if err.Detail == "" {
		return err.Field
	}
	return err.Field + " " + err.Detail
}

func (err *ConfigError) Unwrap() error { return ErrInvalidTrackerConfig }
