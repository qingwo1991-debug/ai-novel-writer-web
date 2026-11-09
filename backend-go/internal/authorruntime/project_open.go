//go:build linux

package authorruntime

import (
	"context"
	"crypto/rand"
	"database/sql"
	"encoding/base64"
	"errors"
	"fmt"
	"time"

	"github.com/qingwo1991-debug/ai-novel-writer-web/backend-go/internal/projectsession"
	"github.com/qingwo1991-debug/ai-novel-writer-web/backend-go/internal/runtime"
	"github.com/qingwo1991-debug/ai-novel-writer-web/backend-go/internal/writerlock"
)

// ProjectOpenResult is the original browser response shape, backed only by a
// verified, private read-only database. It does not grant a writable project.
type ProjectOpenResult struct {
	Success           bool           `json:"success"`
	Project           map[string]any `json:"project"`
	RequestToken      string         `json:"requestToken"`
	ActiveProjectPath *string        `json:"activeProjectPath"`
	DatabaseRestored  bool           `json:"databaseRestored"`
	DBReady           bool           `json:"dbReady"`
	Stale             bool           `json:"stale,omitempty"`
	Error             string         `json:"error,omitempty"`
}

// OpenProjectReadOnly stages and reads project_core before committing a new
// active lease. A missing/malformed core row therefore cannot replace project A.
// The only target authority is the process-local catalog key.
func (l *ReadLifecycle) OpenProjectReadOnly(ctx context.Context, owner projectsession.Owner, key, requestToken string) (ProjectOpenResult, error) {
	if l == nil || l.active == nil || l.catalog == nil || ctx == nil || key == "" || requestToken == "" {
		return ProjectOpenResult{}, ErrInvalid
	}
	var staged map[string]any
	identity, err := l.active.Open(ctx, owner, key, requestToken, func(ctx context.Context, held *writerlock.Lock) (projectsession.Admitted, error) {
		project, err := l.catalog.Resolve(key)
		if err != nil {
			return nil, err
		}
		path, id := project.Path(), project.ProjectID()
		if err := project.Close(); err != nil {
			return nil, err
		}
		config := l.config
		config.ProjectPath = path
		reader, err := OpenReadOnlyUnderLock(ctx, held, l.catalog, key, config)
		if err != nil {
			return nil, err
		}
		closeFailed := true
		defer func() {
			if closeFailed {
				_ = reader.Close()
			}
		}()
		report, err := reader.Report()
		if err != nil || report.ProjectID != id {
			return nil, errors.Join(ErrChanged, err)
		}
		var nonce [24]byte
		if _, err := rand.Read(nonce[:]); err != nil {
			return nil, err
		}
		candidate := projectsession.Context{ProjectID: id, ProjectPath: path, LeaseID: base64.RawURLEncoding.EncodeToString(nonce[:])}
		if err := reader.Read(ctx, func(tx *sql.Tx) error {
			staged, err = readProjectOpenCore(ctx, tx, candidate)
			return err
		}); err != nil {
			return nil, err
		}
		closeFailed = false
		return &admittedRead{reader: reader, context: candidate}, nil
	})
	if err != nil {
		result := ProjectOpenResult{RequestToken: requestToken, Error: redactOpenError(err), Stale: errors.Is(err, projectsession.ErrStaleOpen) || errors.Is(err, runtime.ErrOwned)}
		current, present, currentErr := l.Current(ctx, owner)
		if currentErr != nil {
			return ProjectOpenResult{}, currentErr
		}
		if present {
			path := current.ProjectPath
			result.ActiveProjectPath = &path
			result.DBReady = true
			result.DatabaseRestored = true
		}
		return result, nil
	}
	// Same-key refresh does not run the staging callback. Read its actual core
	// under the returned lease; a new candidate already passed this read before
	// its lease was committed.
	project := staged
	if project == nil {
		if err := l.Read(ctx, owner, identity, func(tx *sql.Tx) error {
			var readErr error
			project, readErr = readProjectOpenCore(ctx, tx, identity)
			return readErr
		}); err != nil {
			return ProjectOpenResult{}, err
		}
	}
	current, present, err := l.Current(ctx, owner)
	if err != nil {
		return ProjectOpenResult{}, err
	}
	if !present || current != identity {
		return ProjectOpenResult{}, projectsession.ErrStaleOpen
	}
	path := identity.ProjectPath
	return ProjectOpenResult{Success: true, Project: project, RequestToken: requestToken,
		ActiveProjectPath: &path, DatabaseRestored: true, DBReady: true}, nil
}

func readProjectOpenCore(ctx context.Context, tx *sql.Tx, identity projectsession.Context) (map[string]any, error) {
	const query = `SELECT project_name, writing_language, genre, sub_genre, target_audience,
		total_chapters, words_per_chapter, creative_strategy, narrative_thread_dormant_threshold,
		plot_structure, narrative_pov, core_outline, world_setting, golden_finger,
		protagonist_profile, global_guidance, writing_style, reference_works,
		character_states, created_at, updated_at FROM project_core WHERE id = 'main'`
	var name, language, genre, subGenre, audience, strategy, structure, pov sql.NullString
	var outline, world, golden, protagonist, guidance, style, references, states sql.NullString
	var created, updated sql.NullString
	var chapters, words, dormant sql.NullInt64
	err := tx.QueryRowContext(ctx, query).Scan(&name, &language, &genre, &subGenre, &audience,
		&chapters, &words, &strategy, &dormant, &structure, &pov, &outline, &world, &golden,
		&protagonist, &guidance, &style, &references, &states, &created, &updated)
	if err != nil {
		return nil, err
	}
	if !name.Valid || !chapters.Valid || !words.Valid || !states.Valid || !created.Valid || !updated.Valid {
		return nil, fmt.Errorf("PROJECT_CORE_INVALID")
	}
	toISO := func(value string) (string, error) {
		for _, layout := range []string{"2006-01-02 15:04:05", time.RFC3339Nano} {
			if instant, err := time.Parse(layout, value); err == nil {
				return instant.UTC().Format("2006-01-02T15:04:05.000Z"), nil
			}
		}
		return "", fmt.Errorf("PROJECT_CORE_TIMESTAMP_INVALID")
	}
	createdAt, err := toISO(created.String)
	if err != nil {
		return nil, err
	}
	updatedAt, err := toISO(updated.String)
	if err != nil {
		return nil, err
	}
	lang := "zh-CN"
	if language.String == "en-US" {
		lang = "en-US"
	}
	creative := strategy.String
	switch creative {
	case "auto", "fluent-drafting", "consistency-first", "deep-planning":
	default:
		creative = "auto"
	}
	threshold := dormant.Int64
	if !dormant.Valid {
		threshold = 3
	} else if threshold < 1 {
		threshold = 1
	} else if threshold > 50 {
		threshold = 50
	}
	return map[string]any{
		"id": identity.ProjectID, "name": name.String, "path": identity.ProjectPath,
		"sessionLease": identity.LeaseID, "characterStates": states.String,
		"createdAt": createdAt, "updatedAt": updatedAt,
		"novelConfig": map[string]any{
			"writingLanguage": lang, "genre": genre.String, "subGenre": subGenre.String,
			"targetAudience": audience.String, "totalChapters": chapters.Int64,
			"wordsPerChapter": words.Int64, "creativeStrategy": creative,
			"narrativeThreadDormantChapterThreshold": threshold, "plotStructure": structure.String,
			"narrativePOV": pov.String, "coreOutline": outline.String, "worldSetting": world.String,
			"goldenFinger": golden.String, "protagonistProfile": protagonist.String,
			"globalGuidance": guidance.String, "writingStyle": style.String,
			"referenceWorks": references.String,
		},
	}, nil
}
