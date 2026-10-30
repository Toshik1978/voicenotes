package capture

import (
	"errors"
	"fmt"
	"net/http"
	"net/url"
)

// Class is the result category of one step attempt (research R10).
type Class int

// Outcome classes.
const (
	// ClassOK means the attempt succeeded.
	ClassOK Class = iota
	// ClassTemporary is retried with backoff: 5xx, 408, 429, network errors, timeouts.
	ClassTemporary
	// ClassMisconfig is retried like temporary, degrades health and never
	// produces a placeholder note: 401, 403, 404.
	ClassMisconfig
	// ClassPermanent will not succeed on retry: 400, 413, 415, 422.
	ClassPermanent
)

func (c Class) String() string {
	switch c {
	case ClassOK:
		return "ok"
	case ClassTemporary:
		return "temporary"
	case ClassMisconfig:
		return "misconfig"
	case ClassPermanent:
		return "permanent"
	default:
		return fmt.Sprintf("class(%d)", int(c))
	}
}

// Outcome is the result of one attempt at a step.
type Outcome struct {
	Class Class
	// Reason is a short, secret-free description (for example "http 503").
	Reason string
	// WaitingOn names the dependency involved, for non-OK classes.
	WaitingOn WaitingOn
}

// On returns a copy of o that names the dependency involved. HTTP clients
// call ClassifyHTTP(status).On(WaitingSTT) and the like.
func (o Outcome) On(w WaitingOn) Outcome {
	o.WaitingOn = w
	return o
}

// OK, Temporary, Misconfig and Permanent build outcomes directly.
func OK() Outcome                     { return Outcome{Class: ClassOK} }
func Temporary(reason string) Outcome { return Outcome{Class: ClassTemporary, Reason: reason} }
func Misconfig(reason string) Outcome { return Outcome{Class: ClassMisconfig, Reason: reason} }
func Permanent(reason string) Outcome { return Outcome{Class: ClassPermanent, Reason: reason} }

// ClassifyHTTP is the single definition of research R10 shared by every HTTP
// client. 2xx is OK; 408, 429 and 5xx are temporary; 401, 403 and 404 are
// misconfiguration; 400, 413, 415 and 422 are permanent. Any other status is
// treated as temporary so that a capture is never dropped on a surprise.
// The caller adds the dependency with Outcome.On.
func ClassifyHTTP(status int) Outcome {
	reason := fmt.Sprintf("http %d", status)
	switch {
	case status >= 200 && status < 300:
		return OK()
	case status == http.StatusUnauthorized, status == http.StatusForbidden, status == http.StatusNotFound:
		return Misconfig(reason)
	case status == http.StatusBadRequest, status == http.StatusRequestEntityTooLarge,
		status == http.StatusUnsupportedMediaType, status == http.StatusUnprocessableEntity:
		return Permanent(reason)
	default:
		return Temporary(reason)
	}
}

// NetworkError marks a transport error or timeout as temporary. A nil error
// yields OK. The reason is secret-free: a *url.Error is reduced to its Op and
// underlying error, because its text embeds the request URL, which may carry
// userinfo, a path or a query token.
func NetworkError(err error) Outcome {
	if err == nil {
		return OK()
	}
	var ue *url.Error
	if errors.As(err, &ue) && ue.Err != nil {
		return Temporary(ue.Op + ": " + ue.Err.Error())
	}

	return Temporary(err.Error())
}
