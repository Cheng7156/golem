package main

import (
	"golem_plugin_hermes/internal/output"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

func classifySendError(err error, ambiguous bool) error {
	if permanentSendCode(status.Code(err)) {
		return output.PermanentError{Err: err}
	}
	if ambiguous {
		return output.AmbiguousError{Err: err}
	}
	return err
}

func permanentSendCode(code codes.Code) bool {
	switch code {
	case codes.InvalidArgument, codes.FailedPrecondition,
		codes.PermissionDenied, codes.Unauthenticated:
		return true
	default:
		return false
	}
}
