package promptskills

import (
	"context"
	"encoding/json"
	"errors"
	"net/url"
	"strings"
	"time"
)

// Caller is constructed by authenticated transport, never from operation args.
// All four identity components are mandatory; Generation scopes app authority.
type Caller struct {
	Principal  string
	SessionID  string
	ClientID   string
	Generation string
}

func (c Caller) valid() bool {
	return c.Principal != "" && c.SessionID != "" && c.ClientID != "" && c.Generation != ""
}

type receiptKey struct {
	Caller    Caller
	SourceURL string
}
type receipt struct {
	SHA256      string
	ResolvedURL string
	Expires     time.Time
	Token       string
}

const InspectionTTL = 10 * time.Minute

var ErrInspectFirst = errors.New("Inspect this GitHub Writing Skill before confirming installation")
var ErrRemoteUnavailable = errors.New("APPROVED_GITHUB_DISPATCH_UNAVAILABLE")

// GitHubDispatcher must enforce endpoint approval, DNS/IP policy, TLS, no
// redirects, timeouts and response limits. There is deliberately no default
// http.Client or process-wide default transport. URLs below are reconstructed
// exclusively from ParseGitHubURL's safe parts. The implementation MUST NOT
// follow redirects or broaden these two origins.
type GitHubDispatcher interface {
	FetchGitHub(context.Context, string, int) (status int, body []byte, err error)
}

// RemoteAuthority must hold the application's current-generation admission read
// lock until release. Lifecycle rollover/shutdown must take its write lock.
// This prevents a checked generation becoming stale before publication.
type RemoteAuthority interface {
	AcquireCurrent(context.Context, Caller) (release func(), err error)
}

func (s *Service) SetRemoteAuthority(authority RemoteAuthority) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return ErrClosed
	}
	if len(s.receipts) != 0 {
		return errors.New("INSPECTION_AUTHORITY_BUSY")
	}
	s.authority = authority
	return nil
}

type RemoteInspection struct {
	Metadata       Metadata `json:"metadata"`
	Compatible     bool     `json:"compatible"`
	Reasons        []string `json:"reasons"`
	SuggestedStage string   `json:"suggestedStage"`
	UTF8Bytes      int      `json:"utf8Bytes"`
	SourceURL      string   `json:"sourceUrl"`
	ResolvedURL    string   `json:"resolvedUrl"`
	ContentSHA256  string   `json:"contentSha256"`
}

func fetchRemote(ctx context.Context, source string, dispatch GitHubDispatcher) (string, RemoteInspection, error) {
	if dispatch == nil {
		return "", RemoteInspection{}, ErrRemoteUnavailable
	}
	if len(source) > 2048 {
		return "", RemoteInspection{}, errors.New("The GitHub URL is invalid")
	}
	location, err := ParseGitHubURL(source)
	if err != nil {
		return "", RemoteInspection{}, err
	}
	ref := location.Ref
	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	if ref == "" {
		endpoint := "https://api.github.com/repos/" + url.PathEscape(location.Owner) + "/" + url.PathEscape(location.Repo)
		status, data, e := dispatch.FetchGitHub(ctx, endpoint, MaxSkillBytes)
		if e != nil {
			return "", RemoteInspection{}, e
		}
		if status < 200 || status > 299 || len(data) > MaxSkillBytes {
			return "", RemoteInspection{}, errors.New("GitHub repository lookup failed")
		}
		var repo struct {
			DefaultBranch string `json:"default_branch"`
		}
		if json.Unmarshal(data, &repo) != nil || !ValidName(repo.DefaultBranch) {
			return "", RemoteInspection{}, errors.New("GitHub returned an unsupported default branch name")
		}
		ref = repo.DefaultBranch
	}
	parts := append([]string{location.Owner, location.Repo, ref}, strings.Split(location.Path, "/")...)
	for i := range parts {
		parts[i] = url.PathEscape(parts[i])
	}
	resolved := "https://raw.githubusercontent.com/" + strings.Join(parts, "/")
	status, data, err := dispatch.FetchGitHub(ctx, resolved, MaxSkillBytes)
	if err != nil {
		return "", RemoteInspection{}, err
	}
	if status < 200 || status > 299 {
		return "", RemoteInspection{}, errors.New("SKILL.md download failed")
	}
	if !validText(data) {
		return "", RemoteInspection{}, errors.New("SKILL.md must be UTF-8 text no larger than 64 KiB")
	}
	inspection := InspectMarkdown(string(data))
	return string(data), RemoteInspection{Metadata: inspection.Metadata, Compatible: inspection.Compatible, Reasons: inspection.Reasons, SuggestedStage: inspection.SuggestedStage, UTF8Bytes: inspection.UTF8Bytes, SourceURL: source, ResolvedURL: resolved, ContentSHA256: digest(data)}, nil
}

// InspectGitHub creates a caller-bound receipt under the unchanged sourceURL
// wire key. A new inspection replaces this caller's previous receipt for URL.
func (s *Service) InspectGitHub(ctx context.Context, caller Caller, source string, dispatch GitHubDispatcher) (RemoteInspection, error) {
	if !caller.valid() {
		return RemoteInspection{}, ErrInspectFirst
	}
	key := receiptKey{caller, source}
	token, err := randomName("")
	if err != nil {
		return RemoteInspection{}, err
	}
	s.mu.Lock()
	if s.closed {
		s.mu.Unlock()
		return RemoteInspection{}, ErrClosed
	}
	if s.authority == nil {
		s.mu.Unlock()
		return RemoteInspection{}, ErrRemoteUnavailable
	}
	release, authorityErr := s.authority.AcquireCurrent(ctx, caller)
	if authorityErr != nil {
		s.mu.Unlock()
		return RemoteInspection{}, authorityErr
	}
	if release == nil {
		s.mu.Unlock()
		return RemoteInspection{}, ErrRemoteUnavailable
	}
	release()
	now := time.Now()
	for k, r := range s.receipts {
		if !now.Before(r.Expires) {
			delete(s.receipts, k)
		}
	}
	if len(s.receipts) >= 1024 {
		s.mu.Unlock()
		return RemoteInspection{}, errors.New("WRITING_SKILL_INSPECTION_LIMIT")
	}
	s.receipts[key] = receipt{Token: token, Expires: now.Add(InspectionTTL)}
	s.mu.Unlock()
	_, inspection, err := fetchRemote(ctx, source, dispatch)
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return RemoteInspection{}, ErrClosed
	}
	if ctx.Err() != nil {
		delete(s.receipts, key)
		return RemoteInspection{}, ctx.Err()
	}
	current, exists := s.receipts[key]
	if !exists || current.Token != token {
		return RemoteInspection{}, ErrInspectFirst
	}
	if err != nil {
		delete(s.receipts, key)
		return RemoteInspection{}, err
	}
	if !time.Now().Before(current.Expires) {
		delete(s.receipts, key)
		return RemoteInspection{}, ErrInspectFirst
	}
	current.SHA256 = inspection.ContentSHA256
	current.ResolvedURL = inspection.ResolvedURL
	s.receipts[key] = current
	return inspection, nil
}

type InstalledSkill struct {
	Name       string `json:"name"`
	Source     string `json:"source"`
	Version    string `json:"version,omitempty"`
	Language   string `json:"language"`
	Compatible bool   `json:"compatible"`
	UTF8Bytes  int    `json:"utf8Bytes"`
}

// InstallGitHub atomically consumes inspection authority before any network or
// directory mutation. Refetch is mandatory; changed bytes/default branch deny.
func (s *Service) InstallGitHub(ctx context.Context, caller Caller, source string, dispatch GitHubDispatcher) (InstalledSkill, error) {
	if !caller.valid() {
		return InstalledSkill{}, ErrInspectFirst
	}
	key := receiptKey{caller, source}
	s.mu.Lock()
	if s.closed {
		s.mu.Unlock()
		return InstalledSkill{}, ErrClosed
	}
	confirmed, exists := s.receipts[key]
	delete(s.receipts, key)
	s.mu.Unlock()
	if !exists || confirmed.SHA256 == "" || !time.Now().Before(confirmed.Expires) {
		return InstalledSkill{}, ErrInspectFirst
	}
	raw, inspection, err := fetchRemote(ctx, source, dispatch)
	if err != nil {
		return InstalledSkill{}, err
	}
	if confirmed.SHA256 != inspection.ContentSHA256 || confirmed.ResolvedURL != inspection.ResolvedURL {
		return InstalledSkill{}, errors.New("The Writing Skill changed after inspection; inspect it again")
	}
	if !inspection.Compatible {
		return InstalledSkill{}, errors.New("This is not a self-contained prompt skill: " + strings.Join(inspection.Reasons, ", "))
	}
	if !ValidName(inspection.Metadata.Name) {
		return InstalledSkill{}, ErrUnsafe
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return InstalledSkill{}, ErrClosed
	}
	if ctx.Err() != nil {
		return InstalledSkill{}, ctx.Err()
	}
	if s.authority == nil {
		return InstalledSkill{}, ErrRemoteUnavailable
	}
	release, authorityErr := s.authority.AcquireCurrent(ctx, caller)
	if authorityErr != nil {
		return InstalledSkill{}, authorityErr
	}
	if release == nil {
		return InstalledSkill{}, ErrRemoteUnavailable
	}
	defer release()
	if ctx.Err() != nil {
		return InstalledSkill{}, ctx.Err()
	}
	if err = s.install(raw, inspection.Metadata.Name, nil, false); err != nil {
		return InstalledSkill{}, err
	}
	return InstalledSkill{Name: inspection.Metadata.Name, Source: "user", Version: inspection.Metadata.Version, Language: inspection.Metadata.Language, Compatible: true, UTF8Bytes: inspection.UTF8Bytes}, nil
}
