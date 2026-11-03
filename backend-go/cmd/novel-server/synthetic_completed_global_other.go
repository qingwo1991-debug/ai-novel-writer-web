//go:build !linux

package main

import (
	"errors"
	"net/http"

	"github.com/qingwo1991-debug/ai-novel-writer-web/backend-go/internal/policy"
)

func migrationHandlerWithCompletedGlobal(_ *policy.Registry, _, _, _ string, _ []string) (http.Handler, func(), error) {
	return nil, nil, errors.New("completed global synthetic lane requires Linux")
}
