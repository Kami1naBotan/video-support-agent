package elasticsearch

import "net/http"

func transientStatus(status int) bool {
	return status == http.StatusRequestTimeout || status == http.StatusTooManyRequests || (status >= 500 && status <= 599)
}

func statusErrorKind(status int) error {
	if transientStatus(status) {
		return ErrUnavailable
	}
	return ErrRejected
}
