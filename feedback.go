package shuttletracker

import (
	"errors"
	"time"
)

// details of each form
type Form struct {
	ID      int64     `json:"id"`
	Message string    `json:"message"`
	Created time.Time `json:"created"`
	Read    bool      `json:"read"`
}

// Feedbackervice is an interface for interacting with Feedback.
type FeedbackService interface {
	Form(id int64) (*Form, error)
	Forms() ([]*Route, error)
	CreateForm(route *Route) error //idk if needs to be added with user input forms
	DeleteForm(id int64) error
}

// ErrFormNotFound indicates that a Form is not found.
var ErrFormNotFound = errors.New("Form not found")