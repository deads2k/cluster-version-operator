package precondition

import (
	"context"
	"fmt"
	"strings"

	"k8s.io/klog"

	"github.com/openshift/cluster-version-operator/pkg/payload"
)

// Error is a wrapper for errors that occur during a precondition check for payload.
type Error struct {
	Nested  error
	Reason  string
	Message string
	Name    string
}

// Error returns the message
func (e *Error) Error() string {
	return e.Message
}

// Cause returns the nested error.
func (e *Error) Cause() error {
	return e.Nested
}

// Precondition defines the precondition check for a payload.
type Precondition interface {
	// Run executes the precondition checks ands returns an error when the precondition fails.
	Run(ctx context.Context, desiredVersion string) error

	// Name returns a human friendly name for the precondition.
	Name() string
}

// List is a list of precondition checks.
type List []Precondition

// RunAll runs all the reflight checks in order, returning a list of errors if any.
// All checks are run, regardless if any one precondition fails.
func (pfList List) RunAll(ctx context.Context, desiredVersion string) []error {
	var errs []error
	for _, pf := range pfList {
		if err := pf.Run(ctx, desiredVersion); err != nil {
			klog.Errorf("Precondition %q failed: %v", pf.Name(), err)
			errs = append(errs, err)
		}
	}
	return errs
}

// Summarize summarizes all the precondition.Error from errs.
func Summarize(errs []error) error {
	if len(errs) == 0 {
		return nil
	}
	var msgs []string
	for _, e := range errs {
		if pferr, ok := e.(*Error); ok {
			msgs = append(msgs, fmt.Sprintf("Precondition %q failed because of %q: %v", pferr.Name, pferr.Reason, pferr.Error()))
			continue
		}
		msgs = append(msgs, e.Error())
	}
	msg := ""
	if len(msgs) == 1 {
		msg = msgs[0]
	} else {
		msg = fmt.Sprintf("Multiple precondition checks failed:\n* %s", strings.Join(msgs, "\n* "))
	}
	return &payload.UpdateError{
		Nested:  nil,
		Reason:  "UpgradePreconditionCheckFailed",
		Message: msg,
		Name:    "PreconditionCheck",
	}
}
