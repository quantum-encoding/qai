-- ============================================================================
-- 24-qai-failure-patterns.surql
--
-- Seeds the failure_pattern taxonomy for qai's `blast_radius` database.
-- This is the collapsed, idempotent, qai-targeted port of files 20/21/22
-- (the trigger backfill from file 22 is inlined here — no separate UPDATE
-- phase needed).
--
-- Apply via:
--   qai patterns import --seed                          (embedded copy)
--   qai patterns import 24-qai-failure-patterns.surql   (file path)
-- or:
--   surreal import --ns quantumencoding --db blast_radius 24-qai-failure-patterns.surql
--
-- DO NOT add `USE NS ... DB ...` here — qai's HTTP client sets the target
-- via headers, matching the policy in internal/blast/schema.surql.
--
-- Idempotent: UPSERT semantics mean re-running this file is safe and brings
-- existing rows up to the canonical content. Wrapped in a transaction so a
-- partial seed (e.g. a single malformed pattern) doesn't leave the taxonomy
-- half-populated.
--
-- Pre-flight: requires 23-qai-extend-blast.surql to have been applied
-- (the failure_pattern table must exist with the closed category ASSERT).
-- ============================================================================

BEGIN TRANSACTION;

-- =================== Billing / adversarial economics ========================

UPSERT failure_pattern:post_billing SET
    name = "Toll booth at the exit (post-billing)", slug = "post_billing", version = 1,
    category = "billing",
    tags = ["happy_path_bias","adversarial_economics"],
    triggers = ["wallet.Deduct","billing.Settle","billing.Charge","provider.Generate","provider.Stream","openai.NewClient","anthropic.NewClient","genai.NewClient"],
    summary = "User is charged only after the upstream provider returns, so a zero-balance attacker gets free compute.",
    detection = "Does this code perform external/billable work BEFORE reserving or holding the user's funds?",
    remediation = "Reserve/hold funds first; do upstream work; settle against the hold afterwards.";

UPSERT failure_pattern:stream_disconnect_billing SET
    name = "Stream-disconnect billing abort", slug = "stream_disconnect_billing", version = 1,
    category = "billing",
    tags = ["concurrency","stream","adversarial_economics"],
    triggers = ["http.Flusher","flusher.Flush","text/event-stream","eventsource","ResponseWriter.Flush"],
    summary = "Billing is tied to the client request context; dropping the TCP connection mid-stream cancels the context and aborts the deduction.",
    detection = "If the client drops the TCP connection mid-stream, will billing reconciliation still execute?",
    remediation = "Decouple settlement from the client request context; reconcile in a context that survives client disconnect.";

UPSERT failure_pattern:negative_money SET
    name = "Negative money printer", slug = "negative_money", version = 1,
    category = "validation",
    tags = ["billing","bounds"],
    triggers = ["strconv.ParseInt","strconv.ParseFloat","strconv.Atoi","math/big","big.NewInt","big.NewFloat","decimal.NewFromString","decimal.NewFromInt"],
    summary = "Math on user-provided metrics (duration, token counts) without sign/bounds checks lets a negative input credit the attacker.",
    detection = "Are all user-provided numbers (duration, tokens, limits) checked for negative values and max bounds before any arithmetic?",
    remediation = "Validate sign and bounds, then clamp, before using any user number in cost math.";

UPSERT failure_pattern:zero_token_refund SET
    name = "Zero-token free ride", slug = "zero_token_refund", version = 1,
    category = "billing",
    tags = ["refund"],
    triggers = ["wallet.Refund","RefundFull","RefundUsage","RestoreBalance","transaction.Reverse","billing.Refund"],
    summary = "On zero output tokens the full reservation is refunded without accounting for input/reasoning compute the provider still charged for.",
    detection = "On a zero-output result, do we still account for non-refundable input/reasoning cost before refunding?",
    remediation = "Refund net of non-refundable upstream charges; never assume zero output means zero cost.";

UPSERT failure_pattern:multimodal_billing_underestimate SET
    name = "Multimodal billing underestimate", slug = "multimodal_billing_underestimate", version = 1,
    category = "billing",
    tags = ["validation","multimodal","cost_estimation","adversarial_economics"],
    triggers = ["estimateCost","countTokens","len(text)","char_count"],
    summary = "Cost is estimated from text character count, ignoring attached media; a pasted video URL bills as a few tokens while the provider processes a 1-hour video.",
    detection = "Does cost-estimation or rate-limiting account for the weight of attached files, URIs, or media rather than just text length?",
    remediation = "Resolve and weight referenced media before charging; estimate from the resolved payload, not the surface text.";

-- =================== SSRF / outbound request trust ==========================

UPSERT failure_pattern:metadata_ssrf SET
    name = "Cloud metadata SSRF", slug = "metadata_ssrf", version = 1,
    category = "ssrf",
    tags = ["blind_trust","cloud_metadata","go"],
    triggers = ["http.DefaultClient","http.NewRequest","http.NewRequestWithContext","axios.create","axios.get","axios.post","node-fetch","got(","reqwest::Client","reqwest::get","urllib.request.urlopen","httpx.AsyncClient","169.254.169.254"],
    summary = "Default HTTP client fetches user-provided URLs, letting an attacker hit 169.254.169.254 to steal service-account tokens.",
    detection = "Does this endpoint fetch a user-provided URL? Is it blocked from resolving 127.0.0.0/8, 10.0.0.0/8 and 169.254.169.254?",
    remediation = "Resolve and validate the target IP against a private/link-local blocklist before fetching; deny on match.";

UPSERT failure_pattern:redirect_ssrf SET
    name = "Redirect-trap SSRF", slug = "redirect_ssrf", version = 1,
    category = "ssrf",
    tags = ["blind_trust","redirect"],
    triggers = ["http.Client{","CheckRedirect","axios.create","maxRedirects","followRedirect","reqwest::redirect::Policy","allow_redirects"],
    summary = "Even with an IP blocklist, default clients follow 301/302 redirects to the metadata server and leak auth headers.",
    detection = "Does this HTTP client follow redirects automatically? If so, is the redirect target re-validated against the SSRF blocklist?",
    remediation = "Disable auto-redirects or re-run SSRF validation on every hop; strip sensitive headers on cross-origin redirects.";

UPSERT failure_pattern:headless_browser_ssrf SET
    name = "Headless-browser SSRF", slug = "headless_browser_ssrf", version = 1,
    category = "ssrf",
    tags = ["scraper","go","dns_rebinding"],
    triggers = ["chromedp.Run","chromedp.Navigate","playwright","puppeteer","puppeteer-core","page.goto","Page.Navigate","browser.newPage"],
    summary = "A scraper passes arbitrary URLs to a headless browser with no DNS/IP validation, reaching the metadata server or internal VPC.",
    detection = "Does the scraper validate the RESOLVED IP (not just the URL) and guard against DNS rebinding before navigating?",
    remediation = "Validate resolved IP against the blocklist; pin DNS; block private and link-local ranges.";

-- =================== Injection / execution contexts =========================

UPSERT failure_pattern:git_arg_injection SET
    name = "Git argument injection", slug = "git_arg_injection", version = 1,
    category = "injection",
    tags = ["rce","go"],
    triggers = ["exec.Command(\"git\"","exec.CommandContext","spawn(\"git\"","child_process.exec","execSync(`git","execa(\"git","git2::Repository","nodegit","simple-git"],
    summary = "A user URL is passed into git's argv without a -- separator, so a value like -u=upload-pack=... is parsed as a flag, enabling RCE.",
    detection = "Is user input separated from flags with -- and never interpretable as command-line options?",
    remediation = "Insert -- before user args; validate URL scheme; avoid the shell entirely.";

UPSERT failure_pattern:ssh_key_newline_injection SET
    name = "SSH key newline injection", slug = "ssh_key_newline_injection", version = 1,
    category = "injection",
    tags = ["go","metadata"],
    triggers = ["compute.SetMetadata","compute.SetCommonInstanceMetadata","gce.SetMetadata","ssh.NewPublicKey","*ssh.PublicKey","authorized_keys","ssh-rsa","ssh-ed25519"],
    summary = "Concatenating a user username and key without stripping newlines lets an attacker inject arbitrary GCE metadata / SSH keys.",
    detection = "Are newlines and control characters stripped from values before they are injected into structured config?",
    remediation = "Validate and escape inputs; reject control characters; never build config by raw string concatenation.";

UPSERT failure_pattern:surrealql_raw_exec SET
    name = "Raw SurrealQL execution", slug = "surrealql_raw_exec", version = 1,
    category = "injection",
    tags = ["surrealdb","go","admin"],
    triggers = ["db.ExecRaw","ExecRaw(","surrealdb.query","surreal.query","Surreal::new","surrealdb.New"],
    summary = "An admin endpoint passes a raw HTTP string straight to ExecRaw(), giving total DB compromise if the token leaks or is CSRF'd.",
    detection = "Does any endpoint pass a raw user string to a query executor (ExecRaw / eval)?",
    remediation = "Parameterise all queries; remove raw-exec endpoints; scope and CSRF-protect anything that survives.";

-- =================== Identity & access management ===========================

UPSERT failure_pattern:oidc_confused_deputy SET
    name = "OIDC confused deputy", slug = "oidc_confused_deputy", version = 1,
    category = "auth",
    tags = ["iam","oidc","go"],
    triggers = ["idtoken.Validate","idtoken.Verify","oidc.NewProvider","oidc.IDTokenVerifier","google.golang.org/api/idtoken","jose.JWS.verify","jwt.Verify","jwt.Parse"],
    summary = "An OIDC verifier checks the signature and audience but ignores the subject/email claim, so any cloud account can trigger internal admin endpoints.",
    detection = "Does token validation assert the SUBJECT/email identity, not just a valid signature and audience?",
    remediation = "Pin the allowed subject/email; verify issuer + audience + subject together.";

UPSERT failure_pattern:oauth_nonce_replay SET
    name = "Sign-in nonce replay", slug = "oauth_nonce_replay", version = 1,
    category = "auth",
    tags = ["iam","apple","replay"],
    triggers = ["appleid.apple.com","apple-auth","apple-signin","@invertase/apple-authentication","verifyAppleToken","ASAuthorizationAppleIDProvider","AppleIDProvider"],
    summary = "Apple JWT validation omits the nonce claim, so a captured login token can be replayed to hijack a session.",
    detection = "Is a per-login nonce generated, bound to the request, and verified to prevent replay?",
    remediation = "Bind and verify a single-use, expiring nonce on every login.";

UPSERT failure_pattern:webhook_signature_stub SET
    name = "Stubbed webhook signature", slug = "webhook_signature_stub", version = 1,
    category = "auth",
    tags = ["iam","webhook","twilio"],
    triggers = ["twilio.RequestValidator","X-Twilio-Signature","stripe.Webhook.constructEvent","Stripe-Signature","crypto.timingSafeEqual","verifyWebhook","validateTwilioRequest"],
    summary = "A webhook signature validator is a placeholder that returns true, so anyone can spoof provider callbacks and run up charges.",
    detection = "Is the webhook signature actually verified against the provider secret (not stubbed to true)?",
    remediation = "Verify the provider HMAC/signature and fail closed on mismatch.";

UPSERT failure_pattern:uds_peercred_spoof SET
    name = "UDS missing SO_PEERCRED", slug = "uds_peercred_spoof", version = 1,
    category = "permission",
    tags = ["iam","ipc","rust","linux"],
    triggers = ["net.Listen(\"unix\"","net.Dial(\"unix\"","UnixListener","UnixStream","tokio::net::UnixListener","tokio::net::UnixStream","SO_PEERCRED","getsockopt"],
    summary = "A Unix-domain-socket approval channel skips SO_PEERCRED, so any local process can spoof an approval and bypass the sandbox.",
    detection = "Does the IPC socket verify peer credentials (SO_PEERCRED) and an expected uid/pid before trusting a request?",
    remediation = "Check SO_PEERCRED and allowlist the caller identity before acting.";

UPSERT failure_pattern:path_prefix_authz SET
    name = "Path-prefix kill switch", slug = "path_prefix_authz", version = 1,
    category = "permission",
    tags = ["blind_trust"],
    triggers = ["\"/internal/","\"/admin/","\"/_/","router.Group(\"/internal","router.Group(\"/admin","mux.HandleFunc(\"/internal","mux.HandleFunc(\"/admin","app.use(\"/internal","app.use(\"/admin"],
    summary = "Internal webhooks live under an /internal/ path prefix and are assumed uncallable externally without real auth middleware.",
    detection = "Is access control enforced by authn/authz middleware rather than by URL-path convention?",
    remediation = "Enforce auth middleware on internal routes; never rely on a path prefix for protection.";

-- =================== Resource exhaustion / DoS ==============================

UPSERT failure_pattern:decompression_bomb SET
    name = "Decompression bomb", slug = "decompression_bomb", version = 1,
    category = "resource_exhaustion",
    tags = ["zip_bomb","oob_hardware"],
    triggers = ["gzip.NewReader","flate.NewReader","zstd.NewReader","compress/gzip","compress/flate","compress/zlib","pako.inflate","pako.ungzip","fflate.gunzip","fflate.inflate","zlib.gunzip","zlib.inflateSync"],
    summary = "A compressed response is read into memory with no size cap, so a tiny gzip expands to gigabytes and OOM-crashes the process.",
    detection = "Is there an absolute byte cap before reading from a compressed or network stream?",
    remediation = "Cap decompressed size and stream with a hard limit; abort past the cap.";

UPSERT failure_pattern:tarball_aggregate_bomb SET
    name = "Tarball aggregate bomb", slug = "tarball_aggregate_bomb", version = 1,
    category = "resource_exhaustion",
    tags = ["zip_bomb","go"],
    triggers = ["archive/tar","tar.NewReader","tarReader.Next","tar-stream","node-tar","extract-tar","tarfile.open"],
    summary = "An extractor caps per-file size but not file count or aggregate extracted size, so 1MB expands to terabytes and fills the disk.",
    detection = "Are total file COUNT and aggregate extracted SIZE capped, not just per-file size?",
    remediation = "Enforce count + aggregate-byte limits; abort extraction on exceed.";

UPSERT failure_pattern:base64_inline_media SET
    name = "Inline Base64 media bomb", slug = "base64_inline_media", version = 1,
    category = "resource_exhaustion",
    tags = ["memory","go","rust"],
    triggers = ["base64.StdEncoding.EncodeToString","Buffer.from(","buffer.toString(\"base64\")","data:image/","data:audio/","data:video/","atob(","btoa("],
    summary = "Large media is passed as inline Base64 JSON, forcing multiple massive allocations during (de)serialization and OOM-crashing clients.",
    detection = "Are large blobs passed by reference (signed URL) rather than inline Base64 in JSON?",
    remediation = "Use signed URLs / streaming for large media; never inline it.";

UPSERT failure_pattern:unauthenticated_endpoints_unlimited SET
    name = "Unrate-limited public endpoints", slug = "unauthenticated_endpoints_unlimited", version = 1,
    category = "rate_limit",
    tags = ["go","brute_force","billing"],
    triggers = ["httprate.Limit","rate.NewLimiter","golang.org/x/time/rate","express-rate-limit","rate-limiter-flexible","tower::limit","tower_governor","slowapi","fastapi-limiter"],
    summary = "A token-bucket limiter covers API keys but leaves public endpoints (auth, contact, webhooks) wide open to brute force and bill run-up.",
    detection = "Are UNauthenticated endpoints rate-limited too, not just keyed ones?",
    remediation = "Apply per-IP/global limits and per-route budgets to all public routes.";

-- =================== Parsing / binary stream boundaries =====================

UPSERT failure_pattern:asn1_length_dos SET
    name = "ASN.1 length DoS", slug = "asn1_length_dos", version = 1,
    category = "parsing",
    tags = ["crypto"],
    triggers = ["encoding/asn1","asn1.Unmarshal","crypto/x509","x509.ParseCertificate","rasn","simple_asn1","rust-asn1"],
    summary = "DER/PEM length fields are trusted without bounds checks, so a malformed cert claiming more bytes than the buffer holds panics.",
    detection = "Is every declared length prefix validated against the actual remaining buffer before reading?",
    remediation = "Bounds-check all length fields; reject anything exceeding the buffer.";

UPSERT failure_pattern:trailing_garbage_decompress SET
    name = "Trailing-garbage decompression", slug = "trailing_garbage_decompress", version = 1,
    category = "parsing",
    tags = ["binary","zlib","pako","oob_hardware","resource_exhaustion"],
    triggers = ["zlib","pako","fflate","inflate","gunzip","inflateRaw"],
    summary = "Feeding bulk byte chunks to a decompressor crosses object boundaries, so the next object header in the trailing bytes triggers a fatal header-check error; the naive byte-at-a-time fix burns 100% CPU.",
    detection = "When passing chunks to a decompression library, does the code respect object/stream boundaries without crashing or falling back to a byte-by-byte loop?",
    remediation = "Track consumed-byte offsets and decompress per object using the format length framing, not arbitrary chunk boundaries.";

-- =================== Memory safety / concurrency / hardware =================

UPSERT failure_pattern:errdefer_leak SET
    name = "errdefer memory leak", slug = "errdefer_leak", version = 1,
    category = "memory",
    tags = ["zig","error_path"],
    triggers = ["allocator.alloc","allocator.create","allocator.dupe","mem.Allocator","std.heap.GeneralPurposeAllocator","ArenaAllocator","FixedBufferAllocator"],
    summary = "Memory allocated in a loop is not freed when an error returns mid-way, leaking the partially allocated chunks.",
    detection = "Does every allocation have a matching defer/errdefer covering all error paths?",
    remediation = "Pair each allocation with errdefer; test the error paths for leaks.";

UPSERT failure_pattern:spinlock_starvation SET
    name = "Spinlock starvation", slug = "spinlock_starvation", version = 1,
    category = "concurrency",
    tags = ["oob_hardware"],
    triggers = ["std.Thread.SpinLock","std.atomic.Atomic","std.atomic.spinLoopHint","@atomicRmw","@atomicLoad","@atomicStore"],
    summary = "A spinlock with no backoff or yield burns 100% CPU on every waiter forever if the lock holder dies.",
    detection = "Do spinlocks have backoff/yield and a holder-death or timeout path?",
    remediation = "Add yield/backoff; prefer an OS mutex; detect a dead holder.";

UPSERT failure_pattern:global_sdk_state_mutation SET
    name = "Global SDK state mutation", slug = "global_sdk_state_mutation", version = 1,
    category = "concurrency",
    tags = ["global_state","sdk"],
    triggers = ["vertexai.init","vertexai.Init","firebase.initializeApp","admin.initializeApp","openai.api_key","anthropic.api_key"],
    summary = "A tool calls a global init (e.g. region setup) inside the request path, silently mutating server-wide routing state.",
    detection = "Does this tool mutate global/process-wide SDK state during a request?",
    remediation = "Scope the client/config per request; keep global init out of request paths.";

UPSERT failure_pattern:idempotency_double_bill SET
    name = "Idempotency double-billing", slug = "idempotency_double_bill", version = 1,
    category = "concurrency",
    tags = ["billing","idempotency","retry","go"],
    triggers = ["gax.WithRetry","retry.Do","backoff.Retry","Stripe-Idempotency-Key","IdempotencyKey","idempotency_key","RetryConfig","exponential-backoff"],
    summary = "An SDK retries a timed-out-but-successful request while the backend does check-then-bill, so the user is billed twice.",
    detection = "Is billing an atomic check-and-set (transaction / idempotency key), not a check-then-act?",
    remediation = "Use an atomic transaction keyed by an idempotency token.";

-- =================== Cryptography ===========================================

UPSERT failure_pattern:memory_address_csprng SET
    name = "Memory-address seeded RNG", slug = "memory_address_csprng", version = 1,
    category = "crypto",
    tags = ["entropy","zig"],
    triggers = ["@intFromPtr","@ptrToInt","std.rand.DefaultPrng.init","std.Random.DefaultPrng.init","std.Random.Sfc64","rand.Seed(time.Now().UnixNano())","math/rand"],
    summary = "An RNG is seeded from stack memory pointers instead of OS entropy, producing predictable UUIDs and session tokens.",
    detection = "Is randomness seeded from an OS CSPRNG, not memory addresses or wall-clock time?",
    remediation = "Use the OS/std.crypto CSPRNG; never seed from pointers.";

UPSERT failure_pattern:secret_not_zeroed SET
    name = "Secret not zeroed before free", slug = "secret_not_zeroed", version = 1,
    category = "crypto",
    tags = ["memory"],
    triggers = ["crypto/rsa","*rsa.PrivateKey","crypto/ed25519","ed25519.PrivateKey","secureZero","zeroize::Zeroize","zeroize::ZeroizeOnDrop","std.crypto","secrecy::Secret"],
    summary = "Memory holding private keys or ADC secrets is freed without secure-zeroing, leaving the secrets recoverable on the heap.",
    detection = "Are cryptographic secrets explicitly zeroed before the memory is freed?",
    remediation = "secureZero/zeroize secret buffers before free; use guarded secret types.";

-- =================== Sandboxing / system architecture =======================

UPSERT failure_pattern:monitor_everything_panic SET
    name = "Monitor-everything kernel panic", slug = "monitor_everything_panic", version = 1,
    category = "sandboxing",
    tags = ["swift","macos","endpoint_security"],
    triggers = ["EndpointSecurity","es_new_client","ES_EVENT_TYPE_AUTH_EXEC","ES_EVENT_TYPE_AUTH_OPEN","es_subscribe","es_client_t","ESClient"],
    summary = "Applying AUTH blocking to every user-tagged process means tagging launchd/kernel_task blocks core OS functions and bricks the system.",
    detection = "Is there an immutable allowlist protecting core OS paths from ever being blocked?",
    remediation = "Maintain an immutable /System/ and /usr/libexec/ allowlist; never block core processes.";

UPSERT failure_pattern:ld_preload_bypass SET
    name = "LD_PRELOAD sandbox bypass", slug = "ld_preload_bypass", version = 1,
    category = "sandboxing",
    tags = ["linux","ebpf"],
    triggers = ["LD_PRELOAD","setenv(\"LD_PRELOAD\"","RTLD_NEXT","dlsym(","LD_LIBRARY_PATH"],
    summary = "Assuming LD_PRELOAD sandboxes an agent fails because a statically linked binary skips the dynamic linker entirely.",
    detection = "Does the sandbox rely on the dynamic linker (LD_PRELOAD) alone, with no kernel-level backstop?",
    remediation = "Add a kernel-level backstop (eBPF/seccomp) beneath the linker-based layer.";

-- =================== UI state ===============================================

UPSERT failure_pattern:global_state_ui_cascade SET
    name = "Global-state render cascade", slug = "global_state_ui_cascade", version = 1,
    category = "ui_state",
    tags = ["swift","render"],
    triggers = ["@EnvironmentObject","@StateObject","@ObservedObject","ObservableObject","@Published"],
    summary = "Binding large views to a single global observable means a small state tick diffs the whole hierarchy and freezes the UI.",
    detection = "Does a small/high-frequency state change invalidate an oversized view subtree?",
    remediation = "Scope observable state narrowly; split large views; isolate high-frequency state.";

-- =================== Distributed / serverless / edge ========================

UPSERT failure_pattern:kv_eventual_consistency_limiter SET
    name = "Eventually-consistent rate limiter", slug = "kv_eventual_consistency_limiter", version = 1,
    category = "distributed",
    tags = ["rate_limit","consistency","cloudflare_kv","serverless","happy_path_bias"],
    triggers = ["cloudflare:kv","KVNamespace","env.KV","kv.put"],
    summary = "A rate limiter built on an eventually-consistent KV store fails open under concurrent bursts (edges read 0) and hits the 1-write/s/key limit with 5xx.",
    detection = "Does this perform rapid sequential writes to a key-value store, and does that store actually provide strong consistency and high write throughput?",
    remediation = "Use a strongly-consistent primitive (Durable Object atomic counter, or D1 with a transaction) for rate limiting; never KV.";

UPSERT failure_pattern:serverless_n_plus_1_io SET
    name = "Serverless N+1 I/O trap", slug = "serverless_n_plus_1_io", version = 1,
    category = "io",
    tags = ["serverless","n_plus_1","latency","performance","cloudflare"],
    triggers = ["R2","S3","r2.get","bucket.get"],
    summary = "Traversing data by issuing one HTTP GET to object storage per iteration blows past the worker wall-time limit and crashes the request.",
    detection = "Is sequential network I/O (R2/S3 fetches) happening inside a loop? Can it be replaced by a single indexed query or batched/parallel fetch?",
    remediation = "Replace per-item fetches with one indexed DB query or a batched/parallel read; precompute an index.";

UPSERT failure_pattern:dangling_distributed_txn SET
    name = "Dangling distributed transaction", slug = "dangling_distributed_txn", version = 1,
    category = "distributed",
    tags = ["saga","ttl","billing","serverless","compensation"],
    triggers = ["D1Database","beginTransaction","deduct","reserve"],
    summary = "A distributed transaction has no TTL or sweeper, so an OOM or dropped network mid-flight locks user funds permanently because cleanup never runs.",
    detection = "If this serverless function dies mid-execution, will the database be left in an unrecoverable state with no cleanup path?",
    remediation = "Make holds expire (TTL) with a sweeper; design as a saga with idempotent compensation/rollback.";

-- =================== Localization / hydration ===============================

UPSERT failure_pattern:i18n_interpolation_hallucination SET
    name = "i18n interpolation hallucination", slug = "i18n_interpolation_hallucination", version = 1,
    category = "i18n",
    tags = ["svelte","interpolation","build_break","hydration"],
    triggers = ["en.ts","locales/","i18n","t(","messages."],
    summary = "Translating a UI dictionary also translates the interpolation variable names, so a placeholder is renamed and breaks compilation and UI hydration.",
    detection = "Does this translation or text-processing touch curly-brace or template variables, and has it been verified the variable names are left untouched?",
    remediation = "Mask placeholders before translation and restore after; assert the variable set is identical pre- and post-translation.";

-- =================== Agentic (LLM behavioural) ==============================
-- These do NOT manifests_in_file. They attach to the orchestration layer
-- (a future `produced_by` edge → generator). Triggers populated only where
-- there's an honest import-level signal.

UPSERT failure_pattern:context_amnesia SET
    name = "Conversation context amnesia", slug = "context_amnesia", version = 1,
    category = "agentic",
    tags = ["context","attention","prompt_assembly"],
    triggers = ["chatHistory","conversation.messages","openai.chat.completions.create","anthropic.messages.create","google.genai.GenerativeModel"],
    summary = "When parsing chat-history JSON, the loop overwrites the prompt with the last user message, silently dropping all prior context.",
    detection = "Does history parsing accumulate every turn, or overwrite the prompt with the last message?",
    remediation = "Append all turns preserving role and order; never reduce history to the last message.";

UPSERT failure_pattern:tool_schema_translation SET
    name = "Tool-schema translation hallucination", slug = "tool_schema_translation", version = 1,
    category = "agentic",
    tags = ["interop","tool_use"],
    triggers = ["tool_choice","tool_use","function_call","functionDeclarations","toolUseBlock","toOpenAITools","toAnthropicTools"],
    summary = "Mapping one provider's tool fields directly to another's triggers 400s on models that expect a different shape.",
    detection = "Is the tool/schema mapping validated per target provider rather than assumed 1:1?",
    remediation = "Use per-provider schema adapters and validate against the target API.";

UPSERT failure_pattern:reviewer_contamination SET
    name = "Reviewer contamination bias", slug = "reviewer_contamination", version = 1,
    category = "agentic",
    tags = ["eval","isolation","self_review"],
    triggers = [],
    summary = "An agent reviewing code it just wrote (or whose reasoning it witnessed) grades itself too leniently.",
    detection = "Is the reviewer a fresh, isolated context with no exposure to authorship or the generation's reasoning?",
    remediation = "Run the reviewer in a clean context; withhold author identity and reasoning trace.";

UPSERT failure_pattern:lazy_capability_nuke SET
    name = "Lazy capability nuke", slug = "lazy_capability_nuke", version = 1,
    category = "agentic",
    tags = ["happy_path_bias","billing"],
    triggers = ["max_tokens:","maxTokens:","max_output_tokens","InsufficientBalance","InsufficientFundsError","BalanceError"],
    summary = "Hitting an InsufficientBalance edge case, the model hardcodes a low max_tokens for everyone instead of computing dynamic fractional limits.",
    detection = "Does the fix degrade capability globally to dodge an edge case instead of handling it dynamically?",
    remediation = "Compute fractional token limits from the actual wallet balance; do not hardcode a global cap.";

UPSERT failure_pattern:reasoning_model_rag_loop SET
    name = "Reasoning-model RAG loop", slug = "reasoning_model_rag_loop", version = 1,
    category = "agentic",
    tags = ["model_selection","cost"],
    triggers = ["o1-preview","o1-mini","o3-","claude-opus-4","claude-opus","gemini-2.5-pro"],
    summary = "Reasoning models get stuck querying docs 15+ times and time out before producing code on structured tasks.",
    detection = "Is a fast model used for structured execution and a reasoning model reserved for planning/audit?",
    remediation = "Route by task type and cap tool-call loops with a hard ceiling.";

UPSERT failure_pattern:big_bang_context_exhaustion SET
    name = "Big-bang context exhaustion", slug = "big_bang_context_exhaustion", version = 1,
    category = "agentic",
    tags = ["context","i18n","scope","silent_truncation"],
    triggers = [],
    summary = "Asked to translate or generate a large file across many targets at once, the model accepts, exhausts its context window, and silently truncates the output halfway.",
    detection = "Is this generation task too large for a single context window? Should it be a map-reduce across sub-agents?",
    remediation = "Decompose into bounded sub-tasks (per language/per section) with an explicit completeness check on reassembly.";

COMMIT TRANSACTION;

-- ----------------------------------------------------------------------------
-- Post-seed sanity
-- ----------------------------------------------------------------------------
SELECT category, count() AS n FROM failure_pattern GROUP BY category ORDER BY n DESC;
SELECT count() AS total, count(IF array::len(triggers ?? []) > 0 THEN 1 ELSE NONE END) AS with_triggers
    FROM failure_pattern GROUP ALL;
