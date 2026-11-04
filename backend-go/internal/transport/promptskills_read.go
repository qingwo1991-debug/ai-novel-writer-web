package transport

import (
	"context"
	"errors"
	"net/http"
	"reflect"

	"github.com/qingwo1991-debug/ai-novel-writer-web/backend-go/internal/promptskills"
	"github.com/qingwo1991-debug/ai-novel-writer-web/backend-go/internal/security"
)

// PromptSkillsReadProvider lends the lane's pinned private prompt and
// installed-skill root for one request and never a host path. It is the same
// service seam BindPromptSkills already uses, so a lane that takes this binder
// cannot be handed the absolute root, a username or a browser-controlled path.
type PromptSkillsReadProvider interface {
	BeginPromptSkills(context.Context, Caller) (*promptskills.Service, func(), error)
}

// BindPromptSkillsReads binds only the two read-only names of the prompt and
// installed-skill family: prompt:load-global and skills:list-user. The three
// mutating names that BindPromptSkills also serves (prompt:save-global,
// prompt:delete-global, skills:uninstall-user) stay unbound, so they keep
// failing closed.
//
// Arity is copied from BindPromptSkills exactly: both names take ZERO
// arguments (promptskills.go:27-28 and :35-36), so there is no project path
// argument and no frozen session context argument to check here. That is the
// difference from the project-family read binders, and it is the part a test
// that only asserts "not 501" cannot see: the browser calls these with
// `{"args":[]}`, so requiring a path or a context would break every caller.
//
// The caller gate is the same one BindPromptSkills applies through its startup
// state: an unidentified caller gets GLOBAL_DATA_NOT_READY. The wire shapes are
// the same shapes the existing binder returns (the prompt template/diagnostic
// object and the managed:// skill list), so absence is an empty object or an
// empty list, never an error.
func (s *Server) BindPromptSkillsReads(provider PromptSkillsReadProvider) error {
	if provider == nil {
		return errors.New("PROMPT_SKILLS_PROVIDER_REQUIRED")
	}
	value := reflect.ValueOf(provider)
	switch value.Kind() {
	case reflect.Chan, reflect.Func, reflect.Interface, reflect.Map, reflect.Pointer, reflect.Slice:
		if value.IsNil() {
			return errors.New("PROMPT_SKILLS_PROVIDER_REQUIRED")
		}
	}
	operations := []struct {
		name  string
		arity int
	}{
		{"prompt:load-global", 0},
		{"skills:list-user", 0},
	}
	handlers := make(map[string]Operation, len(operations))
	for _, operation := range operations {
		name, arity := operation.name, operation.arity
		handlers[name] = func(ctx context.Context, caller Caller, args []any) (any, error) {
			if len(args) != arity {
				return nil, security.NewHTTPError(http.StatusBadRequest, "INVALID_ARGUMENTS")
			}
			// This lane has no startup capture, so it must not invent a
			// migration proof to reach the root. An unidentified caller is
			// refused with the same code the startup gate above returns.
			if startupCaller(caller) == "" {
				return nil, security.NewHTTPError(http.StatusConflict, "GLOBAL_DATA_NOT_READY")
			}
			if ctx == nil {
				return nil, errors.New("PROMPT_SKILLS_READ_FAILED")
			}
			service, release, err := provider.BeginPromptSkills(ctx, caller)
			if release != nil {
				defer release()
			}
			if err != nil {
				return nil, promptSkillsReadError(err)
			}
			if service == nil || release == nil {
				return nil, errors.New("PROMPT_SKILLS_PROVIDER_UNAVAILABLE")
			}
			switch name {
			case "prompt:load-global":
				result, err := service.LoadPrompts()
				if err != nil {
					return nil, promptSkillsReadError(err)
				}
				return result, nil
			case "skills:list-user":
				skills, err := service.ListUser()
				if err != nil {
					return nil, promptSkillsReadError(err)
				}
				return promptSkillsListWire(skills), nil
			}
			return nil, errors.New("PROMPT_SKILLS_READ_FAILED")
		}
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, operation := range operations {
		if _, ok := s.registry.Lookup(operation.name); !ok {
			return errors.New("OPERATION_NOT_EXPOSED")
		}
		if _, ok := s.handlers[operation.name]; ok {
			return errors.New("OPERATION_ALREADY_BOUND")
		}
	}
	for name, handler := range handlers {
		s.handlers[name] = handler
	}
	return nil
}

// promptSkillsListWire is the installed-skill projection shared by
// BindPromptSkills and BindPromptSkillsReads. Host paths are not browser
// capabilities: the browser receives stable display identifiers, never the
// absolute root or a username, while generation reads keep the private
// anchored source.
func promptSkillsListWire(skills []promptskills.UserSkill) []any {
	public := make([]any, 0, len(skills))
	for _, skill := range skills {
		bundle := map[string]any{"status": skill.Bundle.Status}
		if skill.Bundle.Status == "intact" {
			files := make([]any, 0, len(skill.Bundle.Files))
			for _, file := range skill.Bundle.Files {
				files = append(files, map[string]any{"path": file.Path, "content": file.Content})
			}
			bundle["files"] = files
		}
		base := "managed://skills/" + skill.Name
		public = append(public, map[string]any{"name": skill.Name, "content": skill.Content, "baseDir": base, "filePath": base + "/SKILL.md", "bundle": bundle})
	}
	return public
}

// promptSkillsReadError keeps these read failures symbolic. The prompt/skill
// root reader may wrap an absolute path, a username or a raw os error, so only
// the finite vocabulary below is ever reflected; nothing else reaches the
// browser verbatim.
func promptSkillsReadError(err error) error {
	if errors.Is(err, context.Canceled) {
		return errors.New("REQUEST_CANCELED")
	}
	if errors.Is(err, context.DeadlineExceeded) {
		return errors.New("REQUEST_DEADLINE_EXCEEDED")
	}
	var public *security.HTTPError
	if errors.As(err, &public) {
		if public == nil {
			return errors.New("PROMPT_SKILLS_READ_FAILED")
		}
		// Rebuild so a provider's own message never becomes public text.
		switch public.Code {
		case "INVALID_ARGUMENTS", "GLOBAL_DATA_NOT_READY":
			return security.NewHTTPError(public.Status, public.Code)
		}
		return errors.New("PROMPT_SKILLS_READ_FAILED")
	}
	// These sentinels are fixed symbols (internal/promptskills/files_linux.go:18-20,
	// internal/promptskills/service.go:20); they never carry a path or a raw error.
	for _, known := range []error{promptskills.ErrUnsafe, promptskills.ErrChanged, promptskills.ErrUnsupported, promptskills.ErrClosed} {
		if errors.Is(err, known) {
			return errors.New(known.Error())
		}
	}
	if err != nil {
		switch err.Error() {
		case "PROMPT_SKILLS_ROOT_UNAVAILABLE", "PROMPT_SKILLS_PROVIDER_UNAVAILABLE", "SYNTHETIC_DURABLE_CLOSED":
			return errors.New(err.Error())
		}
	}
	return errors.New("PROMPT_SKILLS_READ_FAILED")
}
