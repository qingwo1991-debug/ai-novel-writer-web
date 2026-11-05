package admission

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
)

// baselineV7SchemaVersion is the contract version this asset materialises.
// Provisioning refuses to run when the pinned contract moves past it, so a
// stale asset can never be stamped onto a new project database.
const baselineV7SchemaVersion = 7

// baselineV7Object is one schema object together with the exact statement text
// SQLite records for it in sqlite_master.
type baselineV7Object struct {
	kind string
	name string
	ddl  string
}

// baselineV7Objects is the pinned v7 schema, in creation order: every table,
// then every index, then the trigger (the pinned schema has no view).
//
// The ddl text is the exact post-execution sqlite_master text the installed
// TypeScript migration lane leaves behind: the v0 baseline in
// electron/migrations/baseline-schema.ts (plus the lazy blueprint tables, the
// character roster metadata and the text metric table it initialises), then the
// M01..M06 steps in electron/migrations/m0*.ts. SQLite records a statement from
// the object name onward and drops IF NOT EXISTS, and the M02 step rebuilds and
// renames characters, so the recorded text is the shipped form and is not
// identical to the input text in those two places. Fingerprints hash the
// recorded form, so the recorded form is what this asset owns.
var baselineV7Objects = []baselineV7Object{
	{kind: "table", name: "blueprint_character_sync_operations", ddl: `CREATE TABLE blueprint_character_sync_operations (
        operation_id TEXT PRIMARY KEY,
        blueprint_commit_operation_id TEXT NOT NULL UNIQUE,
        blueprint_commit_payload_hash TEXT NOT NULL,
        status TEXT NOT NULL DEFAULT 'pending' CHECK(status IN ('pending', 'completed')),
        start_chapter INTEGER NOT NULL,
        end_chapter INTEGER NOT NULL,
        character_sync_input TEXT NOT NULL,
        completion_receipt TEXT DEFAULT NULL,
        created_at TEXT NOT NULL DEFAULT (datetime('now')),
        updated_at TEXT NOT NULL DEFAULT (datetime('now')),
        completed_at TEXT DEFAULT NULL,
        FOREIGN KEY (blueprint_commit_operation_id)
          REFERENCES blueprint_commit_operations(operation_id) ON DELETE CASCADE
      )`},
	{kind: "table", name: "blueprint_commit_operations", ddl: `CREATE TABLE blueprint_commit_operations (
        operation_id TEXT PRIMARY KEY,
        payload_hash TEXT NOT NULL,
        mode TEXT NOT NULL CHECK(mode IN ('full', 'replace-range')),
        start_chapter INTEGER NOT NULL,
        end_chapter INTEGER NOT NULL,
        character_sync_input TEXT NOT NULL,
        committed_at TEXT NOT NULL DEFAULT (datetime('now'))
      )`},
	{kind: "table", name: "blueprints", ddl: `CREATE TABLE blueprints (
      chapter_number INTEGER PRIMARY KEY,         -- 章节序号
      title TEXT NOT NULL DEFAULT '',             -- 章节标题
      role TEXT DEFAULT '',                       -- 章节角色
      purpose TEXT DEFAULT '',                    -- 核心目的
      key_events TEXT DEFAULT '',                 -- 关键事件
      characters TEXT DEFAULT '[]',               -- 出场角色 (JSON Array)
      suspense_hook TEXT DEFAULT '',              -- 悬念钩子
      user_guidance TEXT DEFAULT '',              -- 用户预设指导
      notes TEXT DEFAULT '',                      -- 后处理提取的章节要点
      notes_updated_at TEXT DEFAULT '',           -- notes 提取时间
      created_at TEXT DEFAULT (datetime('now')),
      updated_at TEXT DEFAULT (datetime('now'))
    )`},
	{kind: "table", name: "chapter_deletion_operations", ddl: `CREATE TABLE chapter_deletion_operations (
      operation_id TEXT PRIMARY KEY,
      draft_id INTEGER NOT NULL UNIQUE,
      chapter_number INTEGER NOT NULL,
      chapter_title TEXT NOT NULL DEFAULT '',
      finalization_id TEXT NOT NULL,
      target_file_name TEXT NOT NULL DEFAULT '',
      knowledge_document_id TEXT NOT NULL DEFAULT '',
      post_process_run_ids TEXT NOT NULL DEFAULT '[]',
      manuscript_status TEXT NOT NULL DEFAULT 'pending',
      manuscript_error TEXT NOT NULL DEFAULT '',
      knowledge_status TEXT NOT NULL DEFAULT 'pending',
      knowledge_error TEXT NOT NULL DEFAULT '',
      legacy_knowledge_authorization TEXT NOT NULL DEFAULT 'not_required',
      legacy_knowledge_authorized_at TEXT NOT NULL DEFAULT '',
      status TEXT NOT NULL DEFAULT 'pending',
      attempt_count INTEGER NOT NULL DEFAULT 0,
      created_at TEXT DEFAULT (datetime('now')),
      updated_at TEXT DEFAULT (datetime('now')),
      completed_at TEXT NOT NULL DEFAULT ''
    )`},
	{kind: "table", name: "character_aliases", ddl: `CREATE TABLE character_aliases (
 character_id TEXT NOT NULL REFERENCES characters(character_id), name TEXT NOT NULL,
 source_key TEXT NOT NULL, valid_from INTEGER NOT NULL, valid_through INTEGER,
 PRIMARY KEY(character_id,name,source_key,valid_from)
)`},
	{kind: "table", name: "character_avatar_assets", ddl: `CREATE TABLE character_avatar_assets (
 character_id TEXT PRIMARY KEY REFERENCES characters(character_id) ON DELETE CASCADE,
 asset_revision INTEGER NOT NULL CHECK(asset_revision > 0),
 relative_path TEXT NOT NULL UNIQUE,
 content_hash TEXT NOT NULL CHECK(length(content_hash) = 64),
 mime TEXT NOT NULL CHECK(mime IN ('image/png','image/jpeg','image/webp')),
 byte_size INTEGER NOT NULL CHECK(byte_size > 0),
 source_reference TEXT NOT NULL DEFAULT ''
)`},
	{kind: "table", name: "character_avatar_unresolved", ddl: `CREATE TABLE character_avatar_unresolved (
 record_id TEXT PRIMARY KEY,
 disposition TEXT NOT NULL CHECK(disposition IN ('orphan','ambiguous','unknown-fork','reference-only')),
 source_reference TEXT NOT NULL,
 source_relative_path TEXT NOT NULL,
 preserved_relative_path TEXT UNIQUE CHECK(CASE WHEN disposition = 'reference-only' THEN preserved_relative_path IS NULL ELSE preserved_relative_path IS NOT NULL END),
 content_hash TEXT CHECK(CASE WHEN disposition = 'reference-only' THEN content_hash IS NULL ELSE content_hash IS NOT NULL AND length(content_hash) = 64 END),
 mime TEXT CHECK(CASE WHEN disposition = 'reference-only' THEN mime IS NULL ELSE mime IS NOT NULL AND mime IN ('image/png','image/jpeg','image/webp','application/octet-stream') END),
 byte_size INTEGER CHECK(CASE WHEN disposition = 'reference-only' THEN byte_size IS NULL ELSE byte_size IS NOT NULL AND byte_size > 0 END),
 candidate_character_ids_json TEXT NOT NULL
)`},
	{kind: "table", name: "character_identity_approvals", ddl: `CREATE TABLE character_identity_approvals (
 operation_id TEXT PRIMARY KEY, payload_hash TEXT NOT NULL, receipt_json TEXT NOT NULL
)`},
	{kind: "table", name: "character_identity_meta", ddl: `CREATE TABLE character_identity_meta (
 id TEXT PRIMARY KEY CHECK(id='main'), revision INTEGER NOT NULL, legacy_count INTEGER NOT NULL
)`},
	{kind: "table", name: "character_identity_origins", ddl: `CREATE TABLE character_identity_origins (
 character_id TEXT PRIMARY KEY REFERENCES characters(character_id), source_key TEXT NOT NULL UNIQUE,
 original_row_json TEXT NOT NULL, original_hash TEXT NOT NULL
)`},
	{kind: "table", name: "character_identity_proposals", ddl: `CREATE TABLE character_identity_proposals (
 proposal_id TEXT PRIMARY KEY, owner_character_id TEXT REFERENCES characters(character_id),
 source_key TEXT NOT NULL, source_hash TEXT NOT NULL, raw_value TEXT NOT NULL, candidate_ids_json TEXT NOT NULL,
 resolved_character_id TEXT REFERENCES characters(character_id), approval_id TEXT REFERENCES character_identity_approvals(operation_id)
)`},
	{kind: "table", name: "character_relationships", ddl: `CREATE TABLE character_relationships (
 relationship_id TEXT PRIMARY KEY,
 source_character_id TEXT NOT NULL REFERENCES characters(character_id),
 target_character_id TEXT NOT NULL REFERENCES characters(character_id),
 relation TEXT NOT NULL, source_display_snapshot TEXT NOT NULL, target_display_snapshot TEXT NOT NULL,
 provenance_json TEXT NOT NULL, approval_id TEXT NOT NULL REFERENCES character_identity_approvals(operation_id)
)`},
	{kind: "table", name: "character_roster_meta", ddl: `CREATE TABLE character_roster_meta (
      id TEXT PRIMARY KEY CHECK (id = 'main'),
      schema_version INTEGER NOT NULL CHECK (schema_version = 1),
      revision INTEGER NOT NULL DEFAULT 0 CHECK (revision >= 0),
      migration_state TEXT NOT NULL CHECK (
        migration_state IN (
          'empty',
          'legacy_cards_preserved',
          'legacy_markdown_pending',
          'ready'
        )
      ),
      legacy_markdown TEXT NOT NULL DEFAULT '',
      projection_hash TEXT NOT NULL DEFAULT '',
      fact_hash TEXT NOT NULL DEFAULT '',
      updated_at TEXT NOT NULL DEFAULT (datetime('now'))
    )`},
	{kind: "table", name: "character_roster_operations", ddl: `CREATE TABLE character_roster_operations (
      operation_id TEXT PRIMARY KEY,
      payload_hash TEXT NOT NULL,
      committed_revision INTEGER NOT NULL,
      projection_hash TEXT NOT NULL,
      created_at TEXT NOT NULL DEFAULT (datetime('now'))
    )`},
	{kind: "table", name: "characters", ddl: `CREATE TABLE "characters" (
      character_id TEXT PRIMARY KEY, legacy_key TEXT, static_provenance TEXT NOT NULL DEFAULT '{}', identity_revision INTEGER NOT NULL DEFAULT 0, retired INTEGER NOT NULL DEFAULT 0 CHECK(retired IN (0,1)), name TEXT,                      -- 角色名
      role TEXT DEFAULT 'supporting',             -- protagonist/antagonist/supporting/minor
      gender TEXT DEFAULT '',
      age TEXT DEFAULT '',
      appearance TEXT DEFAULT '',                 -- 外貌
      personality TEXT DEFAULT '',                -- 性格
      background TEXT DEFAULT '',                 -- 背景
      abilities TEXT DEFAULT '',                  -- 能力
      motivation TEXT DEFAULT '',                 -- 动机
      relationships TEXT DEFAULT '',              -- 关系链
      arc TEXT DEFAULT '',                        -- 弧光
      notes TEXT DEFAULT '',                      -- 备忘录
      cs_location TEXT DEFAULT '',                -- 当前位置
      cs_power_level TEXT DEFAULT '',             -- 修为境界
      cs_physical_state TEXT DEFAULT '',          -- 身体状态
      cs_mental_state TEXT DEFAULT '',            -- 心理状态
      cs_key_items TEXT DEFAULT '',               -- 关键道具
      cs_recent_events TEXT DEFAULT '',           -- 最近事件
      cs_updated_at_chapter INTEGER DEFAULT NULL, -- 状态更新于第几章；NULL = 无 currentState
      cs_provenance TEXT NOT NULL DEFAULT '{}',   -- currentState 字段级来源
      created_at TEXT DEFAULT (datetime('now')),
      updated_at TEXT DEFAULT (datetime('now'))
    )`},
	{kind: "table", name: "consistency_exemptions", ddl: `CREATE TABLE consistency_exemptions (
      stable_fact_key TEXT PRIMARY KEY,
      reason TEXT NOT NULL,
      revoked INTEGER NOT NULL DEFAULT 0 CHECK(revoked IN (0, 1))
    )`},
	{kind: "table", name: "contents", ddl: `CREATE TABLE contents (
      id INTEGER PRIMARY KEY AUTOINCREMENT,
      body TEXT NOT NULL DEFAULT '',              -- 正文/报告内容
      created_at TEXT DEFAULT (datetime('now'))
    )`},
	{kind: "table", name: "continuity_projection_meta", ddl: `CREATE TABLE continuity_projection_meta (
      id TEXT PRIMARY KEY CHECK (id = 'main'),
      generation INTEGER NOT NULL DEFAULT 0 CHECK (generation >= 0),
      stale_from_chapter INTEGER DEFAULT NULL CHECK (stale_from_chapter IS NULL OR stale_from_chapter > 0)
    )`},
	{kind: "table", name: "drafts", ddl: `CREATE TABLE drafts (
      id INTEGER PRIMARY KEY AUTOINCREMENT,
      chapter_number INTEGER NOT NULL,            -- 归属章节
      version INTEGER NOT NULL,                   -- v1, v2...
      status TEXT DEFAULT 'draft',                -- draft/revised/finalized/archived
      source TEXT DEFAULT 'write',                -- write/rewrite
      content_id INTEGER NOT NULL,                -- FK -> contents
      word_count INTEGER DEFAULT 0,               -- 字数缓存
      source_dependencies TEXT NOT NULL DEFAULT '[]', -- ordered draft ids + frozen prose hashes
      created_at TEXT DEFAULT (datetime('now')),
      updated_at TEXT DEFAULT (datetime('now')),
      FOREIGN KEY (content_id) REFERENCES contents(id) ON DELETE RESTRICT
    )`},
	{kind: "table", name: "finalization_outbox", ddl: `CREATE TABLE finalization_outbox (
      finalization_id TEXT PRIMARY KEY,
      draft_id INTEGER NOT NULL UNIQUE,
      chapter_number INTEGER NOT NULL,
      chapter_title TEXT NOT NULL DEFAULT '',
      content_hash TEXT NOT NULL,
      content_revision INTEGER NOT NULL,
      content_snapshot TEXT NOT NULL DEFAULT '',
      target_file_name TEXT NOT NULL,
      knowledge_document_id TEXT NOT NULL DEFAULT '',
      publication_status TEXT NOT NULL DEFAULT 'pending',
      last_error TEXT NOT NULL DEFAULT '',
      published_at TEXT DEFAULT NULL,
      created_at TEXT DEFAULT (datetime('now')),
      updated_at TEXT DEFAULT (datetime('now')),
      FOREIGN KEY (draft_id) REFERENCES drafts(id) ON DELETE CASCADE
    )`},
	{kind: "table", name: "finalized_draft_import_operations", ddl: `CREATE TABLE finalized_draft_import_operations (
      operation_id TEXT PRIMARY KEY,
      payload_hash TEXT NOT NULL,
      receipt_json TEXT NOT NULL,
      created_at TEXT DEFAULT (datetime('now'))
    )`},
	{kind: "table", name: "generation_artifacts", ddl: `CREATE TABLE generation_artifacts (
 artifact_id TEXT PRIMARY KEY, attempt_id TEXT NOT NULL UNIQUE REFERENCES generation_attempts(attempt_id),
 run_id TEXT NOT NULL REFERENCES generation_runs(run_id), artifact_json TEXT NOT NULL,
 revision INTEGER NOT NULL, status TEXT NOT NULL DEFAULT 'partial'
)`},
	{kind: "table", name: "generation_attempts", ddl: `CREATE TABLE generation_attempts (
 attempt_id TEXT PRIMARY KEY, reservation_id TEXT NOT NULL UNIQUE,
 run_id TEXT NOT NULL REFERENCES generation_runs(run_id), root_action_id TEXT NOT NULL REFERENCES generation_roots(root_action_id),
 attempt_json TEXT NOT NULL, usage_receipt_json TEXT, invocation_nonce TEXT NOT NULL,
 UNIQUE(run_id, invocation_nonce)
)`},
	{kind: "table", name: "generation_roots", ddl: `CREATE TABLE generation_roots (
 root_action_id TEXT PRIMARY KEY, idempotency_key TEXT NOT NULL UNIQUE,
 action_json TEXT NOT NULL, budget_json TEXT NOT NULL,
 active_elapsed_ms INTEGER NOT NULL DEFAULT 0, active_since_ms INTEGER,
 blocked_code TEXT, revision INTEGER NOT NULL DEFAULT 0
)`},
	{kind: "table", name: "generation_runs", ddl: `CREATE TABLE generation_runs (
 run_id TEXT PRIMARY KEY, root_action_id TEXT NOT NULL REFERENCES generation_roots(root_action_id),
 binding_json TEXT NOT NULL, status TEXT NOT NULL, created_at_ms INTEGER NOT NULL, open_key TEXT NOT NULL UNIQUE
)`},
	{kind: "table", name: "import_effect_ledger", ddl: `CREATE TABLE import_effect_ledger (
 run_id TEXT NOT NULL REFERENCES import_runs(id) ON DELETE CASCADE,
 stage TEXT NOT NULL,
 batch_id TEXT NOT NULL,
 effect_namespace TEXT NOT NULL,
 effect_key TEXT NOT NULL,
 state TEXT NOT NULL CHECK(state IN ('complete','partial','unknown')),
 evidence_kind TEXT NOT NULL CHECK(evidence_kind IN ('receipt','knowledge-receipt','legacy-checkpoint','terminal')),
 diagnostic_code TEXT NOT NULL DEFAULT '',
 created_at TEXT NOT NULL DEFAULT (datetime('now')),
 updated_at TEXT NOT NULL DEFAULT (datetime('now')),
 PRIMARY KEY(run_id,stage,batch_id),
 UNIQUE(effect_namespace,effect_key),
 CHECK((state='unknown' AND diagnostic_code<>'') OR (state<>'unknown'))
)`},
	{kind: "table", name: "import_global_fact_operations", ddl: `CREATE TABLE import_global_fact_operations (
      operation_id TEXT PRIMARY KEY,
      payload_hash TEXT NOT NULL,
      receipt_json TEXT NOT NULL,
      created_at TEXT NOT NULL DEFAULT (datetime('now'))
    )`},
	{kind: "table", name: "import_legacy_identity_bridge", ddl: `CREATE TABLE import_legacy_identity_bridge (
      id TEXT PRIMARY KEY,
      ciphertext_hex TEXT NOT NULL,
      iv_hex TEXT NOT NULL,
      auth_tag_hex TEXT NOT NULL,
      created_at TEXT NOT NULL DEFAULT (datetime('now'))
    )`},
	{kind: "table", name: "import_reference_documents", ddl: `CREATE TABLE import_reference_documents (
      document_id TEXT PRIMARY KEY,
      idempotency_key_hash TEXT NOT NULL UNIQUE,
      content_hash TEXT NOT NULL,
      chunk_set_hash TEXT NOT NULL,
      expected_chunk_count INTEGER NOT NULL,
      corpus_kind TEXT NOT NULL CHECK(corpus_kind = 'reference'),
      state TEXT NOT NULL DEFAULT 'prepared' CHECK(state IN ('prepared', 'committed')),
      created_at TEXT NOT NULL DEFAULT (datetime('now')),
      updated_at TEXT NOT NULL DEFAULT (datetime('now'))
    )`},
	{kind: "table", name: "import_run_chapters", ddl: `CREATE TABLE import_run_chapters (
      run_id TEXT NOT NULL,
      chapter_number INTEGER NOT NULL,
      source_id TEXT NOT NULL DEFAULT '',
      source_chapter_number INTEGER NOT NULL DEFAULT 0,
      title TEXT NOT NULL DEFAULT '',
      content_fingerprint TEXT NOT NULL,
      content_size INTEGER NOT NULL,
      content_snapshot TEXT NOT NULL,
      PRIMARY KEY (run_id, chapter_number),
      FOREIGN KEY (run_id) REFERENCES import_runs(id) ON DELETE CASCADE
    )`},
	{kind: "table", name: "import_run_knowledge_receipts", ddl: `CREATE TABLE import_run_knowledge_receipts (
      run_id TEXT NOT NULL,
      chapter_number INTEGER NOT NULL,
      purpose TEXT NOT NULL,
      source_id TEXT NOT NULL,
      source_chapter_number INTEGER NOT NULL,
      content_fingerprint TEXT NOT NULL,
      document_id TEXT NOT NULL,
      state TEXT NOT NULL CHECK(state = 'committed'),
      created_at TEXT NOT NULL DEFAULT (datetime('now')),
      updated_at TEXT NOT NULL DEFAULT (datetime('now')),
      PRIMARY KEY (run_id, chapter_number),
      FOREIGN KEY (run_id, chapter_number)
        REFERENCES import_run_chapters(run_id, chapter_number) ON DELETE CASCADE
    )`},
	{kind: "table", name: "import_run_receipts", ddl: `CREATE TABLE import_run_receipts (
      run_id TEXT NOT NULL,
      schema_version INTEGER NOT NULL DEFAULT 1,
      effect_namespace TEXT NOT NULL,
      effect_key TEXT NOT NULL,
      stage TEXT NOT NULL,
      batch_id TEXT NOT NULL,
      kind TEXT NOT NULL,
      payload_json TEXT NOT NULL,
      payload_hash TEXT NOT NULL,
      state TEXT NOT NULL DEFAULT 'prepared' CHECK(state IN ('prepared', 'committed')),
      effect_receipt_json TEXT DEFAULT NULL,
      created_at TEXT NOT NULL DEFAULT (datetime('now')),
      updated_at TEXT NOT NULL DEFAULT (datetime('now')),
      PRIMARY KEY (run_id, stage, batch_id),
      UNIQUE (effect_namespace, effect_key),
      FOREIGN KEY (run_id) REFERENCES import_runs(id) ON DELETE CASCADE
    )`},
	{kind: "table", name: "import_run_source_chapters", ddl: `CREATE TABLE import_run_source_chapters (
      run_id TEXT NOT NULL,
      source_id TEXT NOT NULL,
      source_chapter_number INTEGER NOT NULL,
      title TEXT NOT NULL,
      content_fingerprint TEXT NOT NULL,
      content_size INTEGER NOT NULL,
      word_count INTEGER NOT NULL,
      content_snapshot TEXT NOT NULL,
      PRIMARY KEY (run_id, source_id, source_chapter_number),
      FOREIGN KEY (run_id, source_id) REFERENCES import_run_sources(run_id, source_id) ON DELETE CASCADE
    )`},
	{kind: "table", name: "import_run_sources", ddl: `CREATE TABLE import_run_sources (
      run_id TEXT NOT NULL,
      source_index INTEGER NOT NULL,
      source_id TEXT NOT NULL,
      source_fingerprint TEXT NOT NULL,
      legacy_source_fingerprint TEXT NOT NULL DEFAULT '',
      display_json TEXT NOT NULL,
      status TEXT NOT NULL DEFAULT 'pending' CHECK(status IN ('pending', 'completed', 'failed')),
      manifest_fingerprint TEXT NOT NULL DEFAULT '',
      chapter_count INTEGER NOT NULL DEFAULT 0,
      content_size INTEGER NOT NULL DEFAULT 0,
      word_count INTEGER NOT NULL DEFAULT 0,
      last_error TEXT NOT NULL DEFAULT '',
      updated_at TEXT NOT NULL DEFAULT (datetime('now')),
      PRIMARY KEY (run_id, source_id),
      UNIQUE (run_id, source_index),
      FOREIGN KEY (run_id) REFERENCES import_runs(id) ON DELETE CASCADE
    )`},
	{kind: "table", name: "import_runs", ddl: `CREATE TABLE import_runs (
      id TEXT PRIMARY KEY,
      purpose TEXT NOT NULL DEFAULT 'reference'
        CHECK(purpose IN ('reference', 'author-manuscript')),
      root_run_id TEXT NOT NULL,
      effect_namespace TEXT NOT NULL,
      source_fingerprint TEXT NOT NULL,
      manifest_fingerprint TEXT NOT NULL,
      authority_fingerprint TEXT NOT NULL DEFAULT '',
      legacy_source_fingerprint TEXT NOT NULL DEFAULT '',
      source_display_json TEXT NOT NULL DEFAULT '[]',
      locale TEXT NOT NULL CHECK(locale IN ('zh-CN', 'en-US')),
      stage TEXT NOT NULL DEFAULT 'knowledge'
        CHECK(stage IN (
          'parsing', 'prepared', 'knowledge', 'global', 'style', 'blueprints',
          'author-commit', 'author-publish', 'author-postprocess',
          'refresh', 'completed'
        )),
      status TEXT NOT NULL DEFAULT 'ready'
        CHECK(status IN ('ready', 'running', 'failed', 'cancelled', 'completed')),
      completed_batches_json TEXT NOT NULL DEFAULT '{}',
      last_error TEXT NOT NULL DEFAULT '',
      resumable INTEGER NOT NULL DEFAULT 1 CHECK(resumable IN (0, 1)),
      cancel_requested INTEGER NOT NULL DEFAULT 0 CHECK(cancel_requested IN (0, 1)),
      execution_owner TEXT NOT NULL DEFAULT '',
      execution_epoch INTEGER NOT NULL DEFAULT 0,
      lease_expires_at INTEGER NOT NULL DEFAULT 0,
      total_chapters INTEGER NOT NULL,
      total_content_size INTEGER NOT NULL DEFAULT 0,
      manifest_chapter_count INTEGER NOT NULL,
      manifest_content_size INTEGER NOT NULL DEFAULT 0,
      manifest_word_count INTEGER NOT NULL DEFAULT 0,
      completed_chapters INTEGER NOT NULL DEFAULT 0,
      base_run_id TEXT DEFAULT NULL,
      created_at TEXT NOT NULL DEFAULT (datetime('now')),
      updated_at TEXT NOT NULL DEFAULT (datetime('now')),
      completed_at TEXT DEFAULT NULL,
      FOREIGN KEY (base_run_id) REFERENCES import_runs(id) ON DELETE SET NULL
    )`},
	{kind: "table", name: "import_source_aliases", ddl: `CREATE TABLE import_source_aliases (
      alias_digest TEXT PRIMARY KEY,
      alias_kind TEXT NOT NULL CHECK(alias_kind IN ('location', 'file')),
      source_id TEXT NOT NULL,
      created_at TEXT NOT NULL DEFAULT (datetime('now'))
    )`},
	{kind: "table", name: "import_source_chapter_map", ddl: `CREATE TABLE import_source_chapter_map (
      purpose TEXT NOT NULL CHECK(purpose IN ('reference', 'author-manuscript')),
      source_id TEXT NOT NULL,
      source_chapter_number INTEGER NOT NULL,
      chapter_number INTEGER NOT NULL,
      PRIMARY KEY (purpose, source_id, source_chapter_number),
      UNIQUE (purpose, chapter_number)
    )`},
	{kind: "table", name: "llm_calls", ddl: `CREATE TABLE llm_calls (
      id INTEGER PRIMARY KEY AUTOINCREMENT,
      model_id TEXT NOT NULL,
      model_name TEXT DEFAULT '',
      purpose TEXT DEFAULT '',
      prompt_tokens INTEGER DEFAULT 0,
      completion_tokens INTEGER DEFAULT 0,
      total_tokens INTEGER DEFAULT 0,
      duration_ms INTEGER DEFAULT 0,
      success INTEGER DEFAULT 1,
      error_message TEXT DEFAULT '',
      created_at TEXT DEFAULT (datetime('now'))
    )`},
	{kind: "table", name: "narrative_thread_confirmations", ddl: `CREATE TABLE narrative_thread_confirmations (
      id INTEGER PRIMARY KEY AUTOINCREMENT,
      plan_id INTEGER NOT NULL,
      draft_id INTEGER NOT NULL,
      event_type TEXT NOT NULL CHECK(event_type IN ('planted', 'progressing', 'resolved', 'abandoned')),
      evidence TEXT NOT NULL,
      reason TEXT NOT NULL DEFAULT '',
      created_at TEXT NOT NULL DEFAULT (datetime('now')),
      FOREIGN KEY (plan_id) REFERENCES narrative_thread_plans(id) ON DELETE CASCADE,
      FOREIGN KEY (draft_id) REFERENCES drafts(id) ON DELETE CASCADE,
      UNIQUE(plan_id, draft_id, event_type, evidence)
    )`},
	{kind: "table", name: "narrative_thread_plans", ddl: `CREATE TABLE narrative_thread_plans (
      id INTEGER PRIMARY KEY AUTOINCREMENT,
      title TEXT NOT NULL,
      type TEXT NOT NULL,
      target_start_chapter INTEGER NOT NULL CHECK(target_start_chapter > 0),
      target_end_chapter INTEGER NOT NULL CHECK(target_end_chapter >= target_start_chapter),
      author_intent TEXT NOT NULL DEFAULT '',
      created_at TEXT NOT NULL DEFAULT (datetime('now')),
      updated_at TEXT NOT NULL DEFAULT (datetime('now'))
    )`},
	{kind: "table", name: "post_process_runs", ddl: `CREATE TABLE post_process_runs (
      id TEXT PRIMARY KEY,                        -- UUID
      trigger_source_type TEXT NOT NULL,           -- chapter_finalize / arch_extract
      trigger_source_id TEXT NOT NULL,             -- 章节号 / draft_id
      source_label TEXT DEFAULT '',               -- UI 标签
      all_critical_passed INTEGER DEFAULT 0,
      created_at TEXT DEFAULT (datetime('now')),
      updated_at TEXT DEFAULT (datetime('now'))
    )`},
	{kind: "table", name: "post_process_steps", ddl: `CREATE TABLE post_process_steps (
      id INTEGER PRIMARY KEY AUTOINCREMENT,
      run_id TEXT NOT NULL,                       -- FK -> post_process_runs
      step_key TEXT NOT NULL,                     -- 步骤标识
      label TEXT DEFAULT '',                      -- 展示名称
      critical INTEGER DEFAULT 0,                 -- 是否关键步骤
      ok INTEGER DEFAULT 0,                       -- 是否完成
      error_msg TEXT DEFAULT '',
      attempt_count INTEGER DEFAULT 0,
      completed_at TEXT DEFAULT '',
      last_attempt_at TEXT DEFAULT '',
      FOREIGN KEY (run_id) REFERENCES post_process_runs(id) ON DELETE CASCADE
    )`},
	{kind: "table", name: "project_core", ddl: `CREATE TABLE project_core (
      id TEXT PRIMARY KEY DEFAULT 'main',
      project_name TEXT NOT NULL DEFAULT '',      -- 小说工程名
      -- [基础定位]
      genre TEXT DEFAULT '',                      -- 核心流派
      sub_genre TEXT DEFAULT '',                  -- 细分流派
      target_audience TEXT DEFAULT '',            -- 目标受众
      total_chapters INTEGER DEFAULT 100,         -- 预计总章数
      words_per_chapter INTEGER DEFAULT 3000,     -- 单章基准字数
      writing_language TEXT NOT NULL DEFAULT 'zh-CN', -- 项目级写作语言
      creative_strategy TEXT NOT NULL DEFAULT 'auto', -- 项目级创作策略
      narrative_thread_dormant_threshold INTEGER NOT NULL DEFAULT 3,
      -- [写作技法]
      plot_structure TEXT DEFAULT 'three_act',    -- 故事模型
      narrative_pov TEXT DEFAULT 'third_limited', -- 叙事视角
      writing_style TEXT DEFAULT '',              -- 文风描述
      reference_works TEXT DEFAULT '',            -- 参考作品
      global_guidance TEXT DEFAULT '',            -- 全局行文指导
      golden_finger TEXT DEFAULT '',              -- 金手指设定
      core_outline TEXT DEFAULT '',               -- 作者配置核心大纲（独立于推演摘要）
      world_setting TEXT DEFAULT '',              -- 作者配置世界设定（独立于架构世界观）
      protagonist_profile TEXT DEFAULT '',        -- 作者配置主角档案（独立于角色名单投影）
      -- [架构四大件]
      premise TEXT DEFAULT '',                    -- 故事前提
      worldbuilding TEXT DEFAULT '',              -- 世界观
      characters_arch TEXT DEFAULT '',            -- 人物群像网络
      synopsis TEXT DEFAULT '',                   -- 情节总大纲
      -- [系统缓存]
      character_states TEXT DEFAULT '',           -- 全书角色动态快照
      plot_tree_snapshot TEXT NOT NULL DEFAULT '',-- 可重建的剧情树派生快照
      created_at TEXT DEFAULT (datetime('now')),
      updated_at TEXT DEFAULT (datetime('now'))
    )`},
	{kind: "table", name: "recovery_candidates", ddl: `CREATE TABLE recovery_candidates (
      candidate_id TEXT PRIMARY KEY,
      run_id TEXT NOT NULL,
      step_id TEXT NOT NULL,
      project_id TEXT NOT NULL,
      chapter_number INTEGER NOT NULL CHECK(chapter_number > 0),
      chapter_title TEXT NOT NULL DEFAULT '',
      source_snapshot TEXT NOT NULL,
      source_hash TEXT NOT NULL,
      source_draft_id INTEGER DEFAULT NULL,
      source_draft_version INTEGER DEFAULT NULL,
      source_draft_identity_captured INTEGER NOT NULL DEFAULT 0
        CHECK(source_draft_identity_captured IN (0, 1)),
      visible_text TEXT NOT NULL,
      content_hash TEXT NOT NULL,
      failure_code TEXT NOT NULL DEFAULT '',
      failure_reason TEXT NOT NULL DEFAULT '',
      status TEXT NOT NULL DEFAULT 'pending'
        CHECK(status IN ('pending', 'continued', 'discarded')),
      replaces_candidate_id TEXT DEFAULT NULL,
      created_at TEXT NOT NULL DEFAULT (datetime('now')),
      resolved_at TEXT DEFAULT NULL,
      FOREIGN KEY (replaces_candidate_id) REFERENCES recovery_candidates(candidate_id)
    )`},
	{kind: "table", name: "review_cycle_merges", ddl: `CREATE TABLE review_cycle_merges (
 cycle_id TEXT PRIMARY KEY REFERENCES review_cycles(cycle_id) ON DELETE CASCADE,
 body TEXT NOT NULL
)`},
	{kind: "table", name: "review_cycles", ddl: `CREATE TABLE review_cycles (
 cycle_id TEXT PRIMARY KEY,
 root_action_id TEXT NOT NULL REFERENCES generation_roots(root_action_id),
 review_id INTEGER NOT NULL UNIQUE REFERENCES reviews(id) ON DELETE CASCADE,
 review_content_hash TEXT NOT NULL,
 confirmation_review_id INTEGER UNIQUE REFERENCES reviews(id),
 confirmation_content_hash TEXT,
 revision_id INTEGER UNIQUE REFERENCES revisions(id),
 source_hash TEXT NOT NULL,
 finding_set_hash TEXT NOT NULL,
 comparison_version INTEGER NOT NULL,
 revision_status TEXT NOT NULL CHECK(revision_status IN ('not-generated','generated','merge-committed')),
 merged_hash TEXT,
 recheck_count INTEGER NOT NULL CHECK(recheck_count IN (0,1)),
 recheck_attempt_id TEXT UNIQUE REFERENCES generation_attempts(attempt_id),
 CHECK((confirmation_review_id IS NULL) = (confirmation_content_hash IS NULL)),
 CHECK((revision_status='not-generated' AND revision_id IS NULL AND merged_hash IS NULL AND recheck_count=0 AND recheck_attempt_id IS NULL)
    OR (revision_status='generated' AND revision_id IS NOT NULL AND merged_hash IS NULL AND recheck_count=0 AND recheck_attempt_id IS NULL)
    OR (revision_status='merge-committed' AND revision_id IS NOT NULL AND merged_hash IS NOT NULL
      AND ((recheck_count=0 AND recheck_attempt_id IS NULL) OR (recheck_count=1 AND recheck_attempt_id IS NOT NULL))))
)`},
	{kind: "table", name: "review_findings", ddl: `CREATE TABLE review_findings (
 cycle_id TEXT NOT NULL REFERENCES review_cycles(cycle_id) ON DELETE CASCADE,
 finding_id TEXT NOT NULL,
 source_project_id TEXT NOT NULL,
 source_epoch TEXT NOT NULL,
 source_id TEXT NOT NULL,
 source_revision INTEGER NOT NULL,
 source_content_hash TEXT NOT NULL,
 span_start INTEGER,
 span_end INTEGER,
 span_unit TEXT,
 excerpt_hash TEXT,
 occurrence INTEGER,
 category TEXT NOT NULL,
 target_id TEXT,
 kind TEXT NOT NULL CHECK(kind IN ('objective','literary')),
 problem_text TEXT NOT NULL,
 expected_text TEXT,
 status TEXT NOT NULL CHECK(status IN ('unverified','unresolved','unknown','resolved','author-waived')),
 evidence_hash TEXT,
 confirmation_item_index INTEGER,
 PRIMARY KEY(cycle_id,finding_id),
 CHECK((status IN ('unverified','author-waived') AND span_start IS NULL AND span_end IS NULL AND span_unit IS NULL
    AND excerpt_hash IS NULL AND occurrence IS NULL AND target_id IS NULL)
   OR (status<>'unverified' AND span_start IS NOT NULL AND span_end IS NOT NULL AND span_unit='utf16-code-unit'
    AND excerpt_hash IS NOT NULL AND occurrence IS NOT NULL AND target_id IS NOT NULL)),
 CHECK((status='author-waived' AND confirmation_item_index IS NOT NULL)
   OR (status<>'author-waived' AND confirmation_item_index IS NULL))
)`},
	{kind: "table", name: "reviews", ddl: `CREATE TABLE reviews (
      id INTEGER PRIMARY KEY AUTOINCREMENT,
      base_draft_id INTEGER NOT NULL,             -- 审查对象 FK
      review_index INTEGER NOT NULL,              -- 审阅顺位
      source_draft_chapter_number INTEGER,        -- 审稿时冻结源稿章节
      source_draft_version INTEGER,               -- 审稿时冻结源稿版本
      source_draft_status TEXT,                   -- 审稿时冻结源稿状态
      source_content TEXT,                        -- 审稿时冻结源稿正文
      content_id INTEGER NOT NULL,                -- FK -> contents
      created_at TEXT DEFAULT (datetime('now')),
      FOREIGN KEY (base_draft_id) REFERENCES drafts(id) ON DELETE CASCADE,
      FOREIGN KEY (content_id) REFERENCES contents(id) ON DELETE RESTRICT
    )`},
	{kind: "table", name: "revisions", ddl: `CREATE TABLE revisions (
      id INTEGER PRIMARY KEY AUTOINCREMENT,
      base_draft_id INTEGER NOT NULL,             -- 父草稿 FK
      revision_index INTEGER NOT NULL,            -- r1, r2
      revision_type TEXT NOT NULL,                -- refine | review-fix
      status TEXT DEFAULT 'pending',              -- pending/merged/discarded
      merged_to_draft_id INTEGER,                 -- 合并产出的新 draft
      user_prompt TEXT DEFAULT '',                -- 用户指导
      review_source_id INTEGER,                   -- 关联审稿 ID
      source_draft_chapter_number INTEGER,        -- 生成时冻结源稿章节
      source_draft_version INTEGER,               -- 生成时冻结源稿版本
      source_draft_status TEXT,                   -- 生成时冻结源稿状态
      source_content TEXT,                        -- 生成时冻结源稿正文
      content_id INTEGER NOT NULL,                -- FK -> contents
      word_count INTEGER DEFAULT 0,               -- 字数缓存
      created_at TEXT DEFAULT (datetime('now')),
      updated_at TEXT DEFAULT (datetime('now')),
      FOREIGN KEY (base_draft_id) REFERENCES drafts(id) ON DELETE CASCADE,
      FOREIGN KEY (content_id) REFERENCES contents(id) ON DELETE RESTRICT
    )`},
	{kind: "table", name: "summary_snapshots", ddl: `CREATE TABLE summary_snapshots (
      id INTEGER PRIMARY KEY AUTOINCREMENT,
      draft_id INTEGER DEFAULT NULL,
      chapter_number INTEGER NOT NULL,
      character_states TEXT DEFAULT '',
      chapter_notes TEXT NOT NULL DEFAULT '',
      continuity_facts TEXT NOT NULL DEFAULT '[]',
      character_state_candidates TEXT NOT NULL DEFAULT '[]',
      source_finalization_id TEXT NOT NULL DEFAULT '',
      source_content_hash TEXT NOT NULL DEFAULT '',
      projection_generation INTEGER NOT NULL DEFAULT 0,
      created_at TEXT DEFAULT (datetime('now')),
      FOREIGN KEY (draft_id) REFERENCES drafts(id) ON DELETE CASCADE
    )`},
	{kind: "table", name: "text_metric_versions", ddl: `CREATE TABLE text_metric_versions (
      metric TEXT PRIMARY KEY,
      version INTEGER NOT NULL,
      updated_at TEXT NOT NULL DEFAULT (datetime('now'))
    )`},
	{kind: "index", name: "idx_blueprint_character_sync_status", ddl: `CREATE INDEX idx_blueprint_character_sync_status
        ON blueprint_character_sync_operations(status, created_at)`},
	{kind: "index", name: "idx_chapter_deletion_status", ddl: `CREATE INDEX idx_chapter_deletion_status
      ON chapter_deletion_operations(status)`},
	{kind: "index", name: "idx_character_avatar_revision", ddl: `CREATE INDEX idx_character_avatar_revision ON character_avatar_assets(asset_revision)`},
	{kind: "index", name: "idx_drafts_chapter", ddl: `CREATE INDEX idx_drafts_chapter ON drafts(chapter_number)`},
	{kind: "index", name: "idx_drafts_chapter_version", ddl: `CREATE UNIQUE INDEX idx_drafts_chapter_version
      ON drafts(chapter_number, version)`},
	{kind: "index", name: "idx_finalization_outbox_status", ddl: `CREATE INDEX idx_finalization_outbox_status
      ON finalization_outbox(publication_status)`},
	{kind: "index", name: "idx_import_effect_ledger_state", ddl: `CREATE INDEX idx_import_effect_ledger_state ON import_effect_ledger(run_id,state,stage)`},
	{kind: "index", name: "idx_import_run_chapters_page", ddl: `CREATE INDEX idx_import_run_chapters_page
      ON import_run_chapters(run_id, chapter_number)`},
	{kind: "index", name: "idx_import_run_knowledge_receipts_affiliation", ddl: `CREATE INDEX idx_import_run_knowledge_receipts_affiliation
      ON import_run_knowledge_receipts(
        purpose, source_id, source_chapter_number, content_fingerprint, state
      )`},
	{kind: "index", name: "idx_import_run_receipts_state", ddl: `CREATE INDEX idx_import_run_receipts_state
      ON import_run_receipts(run_id, state, stage)`},
	{kind: "index", name: "idx_import_run_source_chapters", ddl: `CREATE INDEX idx_import_run_source_chapters
      ON import_run_source_chapters(run_id, source_id, source_chapter_number)`},
	{kind: "index", name: "idx_import_runs_purpose_source_status", ddl: `CREATE INDEX idx_import_runs_purpose_source_status
      ON import_runs(purpose, source_fingerprint, status, updated_at)`},
	{kind: "index", name: "idx_import_runs_resumable", ddl: `CREATE INDEX idx_import_runs_resumable
      ON import_runs(resumable, status, updated_at)`},
	{kind: "index", name: "idx_import_runs_source_status", ddl: `CREATE INDEX idx_import_runs_source_status
      ON import_runs(source_fingerprint, status, updated_at)`},
	{kind: "index", name: "idx_import_source_aliases_source", ddl: `CREATE INDEX idx_import_source_aliases_source
      ON import_source_aliases(source_id)`},
	{kind: "index", name: "idx_llm_calls_time", ddl: `CREATE INDEX idx_llm_calls_time ON llm_calls(created_at)`},
	{kind: "index", name: "idx_narrative_thread_confirmations_plan", ddl: `CREATE INDEX idx_narrative_thread_confirmations_plan
      ON narrative_thread_confirmations(plan_id, draft_id, id)`},
	{kind: "index", name: "idx_post_runs_source", ddl: `CREATE INDEX idx_post_runs_source
      ON post_process_runs(trigger_source_type, trigger_source_id)`},
	{kind: "index", name: "idx_recovery_candidates_pending", ddl: `CREATE INDEX idx_recovery_candidates_pending
      ON recovery_candidates(status, created_at)`},
	{kind: "index", name: "idx_reviews_draft_index", ddl: `CREATE UNIQUE INDEX idx_reviews_draft_index
      ON reviews(base_draft_id, review_index)`},
	{kind: "index", name: "idx_revisions_draft_index", ddl: `CREATE UNIQUE INDEX idx_revisions_draft_index
      ON revisions(base_draft_id, revision_index)`},
	{kind: "index", name: "idx_summary_snapshots_draft", ddl: `CREATE UNIQUE INDEX idx_summary_snapshots_draft
      ON summary_snapshots(draft_id) WHERE draft_id IS NOT NULL
  `},
	{kind: "trigger", name: "retain_character_avatar_on_delete", ddl: `CREATE TRIGGER retain_character_avatar_on_delete BEFORE DELETE ON characters
WHEN EXISTS(SELECT 1 FROM character_avatar_assets WHERE character_id=OLD.character_id)
BEGIN
 INSERT OR IGNORE INTO character_avatar_unresolved(
  record_id,disposition,source_reference,source_relative_path,preserved_relative_path,
  content_hash,mime,byte_size,candidate_character_ids_json
 ) SELECT 'character-delete:' || OLD.character_id || ':' || asset_revision,
  'orphan','character-delete:' || OLD.character_id,relative_path,relative_path,
  content_hash,mime,byte_size,json_array(OLD.character_id)
 FROM character_avatar_assets WHERE character_id=OLD.character_id;
END`},
}

// BaselineV7Statements returns the complete pinned v7 DDL as one ordered list of
// single-object statements, ready for a caller that provisions an empty
// database itself: 51 tables, 22 indexes and 1 trigger, in creation order.
//
// The returned slice is a copy; a caller cannot mutate the package asset.
// Applying it to an empty database and stamping PRAGMA user_version = 7
// produces a project database admission accepts. That claim is not trusted on
// inspection: TestBaselineV7DDLReproducesPinnedFingerprint rebuilds the schema
// and asserts the digest the pinned contract in pinned_contract.go holds, so
// the test is what keeps this asset honest.
//
// Callers that would rather have the version stamped for them, and that want
// the empty-database guard, should use WriteBaselineV7.
func BaselineV7Statements() []string {
	statements := make([]string, len(baselineV7Objects))
	for index, object := range baselineV7Objects {
		statements[index] = object.ddl
	}
	return statements
}

// WriteBaselineV7 stamps the pinned v7 schema onto a brand-new author database
// and sets PRAGMA user_version = 7.
//
// dst is the file db was opened from. It must be non-empty because another
// caller provisioning a project database needs a real, exclusive path and not
// an anonymous handle. db must be the open handle for dst, already configured
// by the caller for its own pooling, journal and foreign key settings; this
// function executes DDL only and opens nothing. The database must still be
// empty: an existing schema object fails closed instead of being silently
// re-written or merged. All objects and the version stamp commit in one
// transaction, so a failure leaves nothing behind.
//
// The produced schema is the one the pinned contract in pinned_contract.go
// admits at version 7. That equality is not assumed:
// TestBaselineV7DDLReproducesPinnedFingerprint rebuilds the database from this
// asset, hashes sqlite_master exactly as admission does, and asserts the digest
// equals the pinned fingerprint. That test is what keeps this asset honest; any
// change here must keep it green or be rejected by it.
func WriteBaselineV7(dst string, db *sql.DB) error {
	return writeBaselineV7(dst, db, baselineV7Objects)
}

// writeBaselineV7 applies the supplied objects. It takes the object list so a
// test can apply a variant of the same asset without exporting a second
// provisioning entry point.
func writeBaselineV7(dst string, db *sql.DB, objects []baselineV7Object) error {
	if dst == "" {
		return errors.New("admission: baseline v7 needs a destination path")
	}
	if db == nil {
		return errors.New("admission: baseline v7 needs an open database handle")
	}
	if CurrentSchemaVersion != baselineV7SchemaVersion {
		return fmt.Errorf("admission: baseline v7 asset is stale: pinned contract version is %d", CurrentSchemaVersion)
	}
	ctx := context.Background()
	var existing int
	if err := db.QueryRowContext(ctx, "SELECT count(*) FROM sqlite_master").Scan(&existing); err != nil {
		return fmt.Errorf("admission: baseline v7 cannot inspect %s: %w", dst, err)
	}
	if existing != 0 {
		return fmt.Errorf("admission: baseline v7 refuses to stamp %s: %d schema objects already exist", dst, existing)
	}
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("admission: baseline v7 cannot begin on %s: %w", dst, err)
	}
	defer func() { _ = tx.Rollback() }()
	for _, object := range objects {
		if _, err := tx.ExecContext(ctx, object.ddl); err != nil {
			return fmt.Errorf("admission: baseline v7 %s %s failed: %w", object.kind, object.name, err)
		}
	}
	if _, err := tx.ExecContext(ctx, fmt.Sprintf("PRAGMA user_version = %d", baselineV7SchemaVersion)); err != nil {
		return fmt.Errorf("admission: baseline v7 cannot stamp version %d: %w", baselineV7SchemaVersion, err)
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("admission: baseline v7 cannot commit %s: %w", dst, err)
	}
	return nil
}
