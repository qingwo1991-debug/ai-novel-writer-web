package projectaccess

import (
 "context"
 "errors"
 "os"
 "strings"
 "testing"

 "github.com/qingwo1991-debug/ai-novel-writer-web/backend-go/internal/projectfs"
)
func TestPreflightRedactsMixedCancellationCleanupErrors(t *testing.T){for _,code:=range []error{context.Canceled,context.DeadlineExceeded}{for _,cleanup:=range []bool{false,true}{raw:=errors.Join(code,&os.PathError{Op:"remove",Path:"synthetic-secret-prose-file-name",Err:os.ErrPermission});if cleanup{raw=errors.Join(raw,projectfs.ErrKnowledgeCleanup)};got:=redact(raw);if !errors.Is(got,code)||strings.Contains(got.Error(),"synthetic-secret")||strings.Contains(got.Error(),"remove"){t.Fatal("raw diagnostic escaped",got)};if errors.Is(got,ErrCleanup)!=cleanup{t.Fatal("cleanup evidence lost",got)}}}}
