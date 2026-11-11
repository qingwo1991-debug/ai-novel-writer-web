package projectfs

import "testing"

// TestBrowserRelativePathAllowsOnlyAuthorVisibleStorage pins the one storage
// subtree the shipped renderer reaches by path. The writing-skill loader and the
// prompt catalog read and write `.ai-novel/skills/**`, `.ai-novel/prompts/**`
// and `.ai-novel/writing-skills.json` through fs:check-exists, fs:read-file,
// fs:list-dir and fs:write-file; before this allowlist every one of those calls
// was refused and a workflow could not even freeze its skill snapshot, while the
// author databases and the knowledge store must stay unreachable.
func TestBrowserRelativePathAllowsOnlyAuthorVisibleStorage(t *testing.T) {
	allowed := []string{
		"manuscript/chapter-1.md",
		".ai-novel/skills",
		".ai-novel/skills/story-long-write/SKILL.md",
		".ai-novel/prompts",
		".ai-novel/prompts/plot-outline.zh-CN.json",
		".ai-novel/writing-skills.json",
	}
	for _, path := range allowed {
		if _, err := BrowserRelativePath(path); err != nil {
			t.Fatalf("BrowserRelativePath(%q) refused an author-visible path: %v", path, err)
		}
	}
	refused := []string{
		".ai-novel",
		".ai-novel/project.db",
		".ai-novel/project.json",
		".ai-novel/knowledge-snapshot.json",
		".ai-novel/knowledge-copies/copy.json",
		".ai-novel/knowledge-vectors.json",
		".ai-novel/skills-private/secret.md",
		".ai-novel/prompts-private/secret.json",
		".hidden/anything.md",
		"/data/workspaces/book/.ai-novel/skills",
		"../escape.md",
		"manuscript/../.ai-novel/project.db",
	}
	for _, path := range refused {
		if _, err := BrowserRelativePath(path); err == nil {
			t.Fatalf("BrowserRelativePath(%q) allowed a path the browser must not reach", path)
		}
	}
}
