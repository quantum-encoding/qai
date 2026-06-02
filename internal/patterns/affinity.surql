-- ============================================================================
-- 25-qai-pattern-role-affinity.surql
--
-- The judgment layer the interceptor (file 30) needs to work on day one,
-- before any edge_case history exists:
--   * applies_to_roles — which file roles each pattern tends to bite, so the
--     agent can fire on "this is a handler" without import signals.
--   * polarity — 'smell' (trigger PRESENT = finding) vs 'control' (for a given
--     role, trigger ABSENT = finding; the trigger list is the expected guard).
--
-- Assignments are deliberate, not tag-sets. A pattern lists a role only if that
-- role is genuinely where it shows up. Run AFTER files 20/21/22/24.
--
-- NOTE ON POLARITY vs the binary model: most security patterns are "you did X
-- (smell trigger) but didn't guard it." Those stay 'smell'; the interceptor's
-- LLM checks remediation-satisfaction against the diff (file 30, step 4) rather
-- than warning on raw trigger presence. Only PURE controls — where the trigger
-- list IS the guard list and absence is the whole finding — get 'control'.
-- ============================================================================

DEFINE FIELD OVERWRITE applies_to_roles ON failure_pattern TYPE option<array<string>>;
DEFINE FIELD OVERWRITE polarity ON failure_pattern TYPE string
  DEFAULT 'smell' ASSERT $value INSIDE ['smell','control'];

BEGIN TRANSACTION;

-- ===== Billing / economics ==================================================
UPSERT failure_pattern:post_billing             SET applies_to_roles = ['handler','data_access'],          polarity = 'smell';
UPSERT failure_pattern:stream_disconnect_billing SET applies_to_roles = ['handler'],                        polarity = 'smell';
UPSERT failure_pattern:negative_money           SET applies_to_roles = ['handler','lib'],                   polarity = 'smell';
UPSERT failure_pattern:zero_token_refund        SET applies_to_roles = ['handler','lib'],                   polarity = 'smell';
UPSERT failure_pattern:multimodal_billing_underestimate SET applies_to_roles = ['handler','lib'],           polarity = 'smell';

-- ===== SSRF =================================================================
UPSERT failure_pattern:metadata_ssrf            SET applies_to_roles = ['handler','sdk_client','data_access'], polarity = 'smell';
UPSERT failure_pattern:headless_browser_ssrf    SET applies_to_roles = ['handler','job'],                   polarity = 'smell';
-- CONTROL: outbound-fetch roles are expected to disable/re-validate redirects;
-- triggers list the redirect-disable symbols, so their ABSENCE is the finding.
UPSERT failure_pattern:redirect_ssrf            SET applies_to_roles = ['handler','sdk_client'],            polarity = 'control';

-- ===== Injection ============================================================
UPSERT failure_pattern:git_arg_injection        SET applies_to_roles = ['handler','job','lib'],             polarity = 'smell';
UPSERT failure_pattern:ssh_key_newline_injection SET applies_to_roles = ['handler','job','data_access'],    polarity = 'smell';
UPSERT failure_pattern:surrealql_raw_exec       SET applies_to_roles = ['handler','data_access'],           polarity = 'smell';

-- ===== Identity & access ====================================================
UPSERT failure_pattern:oidc_confused_deputy     SET applies_to_roles = ['middleware','handler'],            polarity = 'smell';
UPSERT failure_pattern:oauth_nonce_replay       SET applies_to_roles = ['middleware','handler'],            polarity = 'smell';
UPSERT failure_pattern:path_prefix_authz        SET applies_to_roles = ['handler','middleware'],            polarity = 'smell';
UPSERT failure_pattern:uds_peercred_spoof       SET applies_to_roles = ['ipc'],                             polarity = 'smell';
-- ^^ REVIEW: uds_peercred_spoof is arguably a 4th CONTROL pattern (ipc role +
--    absence of an SO_PEERCRED / getpeereid check = finding). Flip polarity to
--    'control' ONLY IF file 22 set its triggers to the GUARD symbols
--    (SO_PEERCRED, getpeereid, peer_cred). If 22 set them to a smell symbol
--    (UnixListener, accept), leave it 'smell'. I can't see file 22's triggers,
--    so this is left conservative — confirm and flip if warranted.
-- CONTROL: a webhook handler is expected to verify the signature; triggers list
-- the verify symbols, absence is the finding.
UPSERT failure_pattern:webhook_signature_stub   SET applies_to_roles = ['handler','middleware'],            polarity = 'control';

-- ===== Resource exhaustion ==================================================
UPSERT failure_pattern:decompression_bomb       SET applies_to_roles = ['handler','parser','sdk_client'],   polarity = 'smell';
UPSERT failure_pattern:tarball_aggregate_bomb   SET applies_to_roles = ['handler','job','parser'],          polarity = 'smell';
UPSERT failure_pattern:base64_inline_media      SET applies_to_roles = ['handler','sdk_client','serializer'], polarity = 'smell';
-- CONTROL: any public handler/middleware is expected to be rate-limited;
-- triggers list the limiter symbols, absence is the finding.
UPSERT failure_pattern:unauthenticated_endpoints_unlimited SET applies_to_roles = ['handler','middleware'], polarity = 'control';

-- ===== Parsing ==============================================================
UPSERT failure_pattern:asn1_length_dos          SET applies_to_roles = ['parser','lib'],                    polarity = 'smell';
UPSERT failure_pattern:trailing_garbage_decompress SET applies_to_roles = ['parser','sdk_client'],          polarity = 'smell';

-- ===== Memory safety ========================================================
UPSERT failure_pattern:errdefer_leak            SET applies_to_roles = ['lib','parser','ipc'],              polarity = 'smell';
UPSERT failure_pattern:spinlock_starvation      SET applies_to_roles = ['lib','ipc'],                       polarity = 'smell';
UPSERT failure_pattern:global_sdk_state_mutation SET applies_to_roles = ['handler','sdk_client','lib'],     polarity = 'smell';

-- ===== Cryptography =========================================================
UPSERT failure_pattern:memory_address_csprng    SET applies_to_roles = ['lib','ipc'],                       polarity = 'smell';
UPSERT failure_pattern:secret_not_zeroed        SET applies_to_roles = ['lib','data_access','ipc'],         polarity = 'smell';

-- ===== Sandboxing ===========================================================
UPSERT failure_pattern:monitor_everything_panic SET applies_to_roles = ['lib','config'],                    polarity = 'smell';
UPSERT failure_pattern:ld_preload_bypass        SET applies_to_roles = ['lib','config','job'],              polarity = 'smell';

-- ===== Distributed / serverless / io ========================================
UPSERT failure_pattern:kv_eventual_consistency_limiter SET applies_to_roles = ['data_access','handler'],    polarity = 'smell';
UPSERT failure_pattern:serverless_n_plus_1_io   SET applies_to_roles = ['handler','data_access','job'],     polarity = 'smell';
UPSERT failure_pattern:dangling_distributed_txn SET applies_to_roles = ['handler','data_access'],           polarity = 'smell';

-- ===== Concurrency ==========================================================
UPSERT failure_pattern:idempotency_double_bill  SET applies_to_roles = ['handler','data_access','sdk_client'], polarity = 'smell';

-- ===== UI / i18n ============================================================
UPSERT failure_pattern:global_state_ui_cascade  SET applies_to_roles = ['ui'],                              polarity = 'smell';
UPSERT failure_pattern:i18n_interpolation_hallucination SET applies_to_roles = ['i18n','ui'],               polarity = 'smell';

-- ===== Agentic (orchestration-tier — NOT diff-time) =========================
-- Empty applies_to_roles AND empty triggers (from files 20/21) means the
-- diff-time interceptor never surfaces these. That is the two-tier split
-- enforcing itself: they belong in Conductor policy, not code review.
UPSERT failure_pattern:context_amnesia          SET applies_to_roles = [], polarity = 'smell';
UPSERT failure_pattern:tool_schema_translation  SET applies_to_roles = [], polarity = 'smell';
UPSERT failure_pattern:reviewer_contamination   SET applies_to_roles = [], polarity = 'smell';
UPSERT failure_pattern:lazy_capability_nuke     SET applies_to_roles = [], polarity = 'smell';
UPSERT failure_pattern:reasoning_model_rag_loop SET applies_to_roles = [], polarity = 'smell';
UPSERT failure_pattern:big_bang_context_exhaustion SET applies_to_roles = [], polarity = 'smell';

COMMIT TRANSACTION;

-- ----------------------------------------------------------------------------
-- Verify
-- ----------------------------------------------------------------------------

-- Control patterns (expect 3, or 4 if uds_peercred_spoof was flipped).
SELECT slug, applies_to_roles FROM failure_pattern WHERE polarity = 'control';

-- Orchestration-tier patterns the interceptor must never raise (expect 6).
SELECT count() FROM failure_pattern WHERE applies_to_roles = [] GROUP ALL;

-- The affinity query the interceptor runs for a handler (sanity: should be a
-- meaningful, not enormous, list).
SELECT slug, name, polarity FROM failure_pattern
WHERE applies_to_roles CONTAINS 'handler' ORDER BY polarity, slug;
