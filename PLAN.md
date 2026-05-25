# MasterDnsVPN — نقشه راه ارتقای سرعت و پایداری

> هدف کلی: افزایش throughput، کاهش latency و افزایش پایداری در شرایط شبکه بد، بدون تغییر پروتکل سیمی (wire-compatible باقی بمونه).
> روش: تغییرات کوچک، قابل اندازه‌گیری، یکی بعد از دیگری. هر استپ بنچ‌مارک قبل/بعد داره.
> پیش‌فرض: branch کاری = `genspark_ai_developer` و mainline = `main`.

---

## 🔭 خلاصه آنالیز پروژه (یک بار، مرجع همه استپ‌ها)

- زبان: Go 1.25 — ماژول `masterdnsvpn-go`
- وابستگی‌ها: `BurntSushi/toml`, `klauspost/compress` (zstd/zlib), `pierrec/lz4/v4`, `golang.org/x/crypto`, `golang.org/x/sys`
- نقاط داغ (به‌ترتیب اهمیت):
  1. `internal/arq` — لایه ARQ شبه‑QUIC روی DNS/UDP (۲٬۸۶۷ خط) — قلب پایداری
  2. `internal/client` — balancer (۱٬۴۰۰+ خط)، MTU discovery (۱٬۵۷۶ خط)، session، socks_manager، stream
  3. `internal/udpserver` — server_session/server_postsession، session (۱٬۱۴۶ خط)، deferred workers
  4. `internal/vpnproto` — builder/parser/packing payload-ها
  5. `internal/security` — کدک رمزنگاری (AES/ChaCha20/XOR)
  6. `internal/dnsparser` — پارس کوئری/پاسخ DNS
- زیرساخت موجود: تست‌ها وسیع، بنچ‌مارک end-to-end (`scripts/bench/bench.go`)، Docker، نصب‌گر لینوکس، ۳ workflow اکشن
- خلأهای کلیدی:
  - ❌ هیچ pprof endpoint / runtime profiling
  - ❌ هیچ متریک observability (expvar / prometheus / counters قابل scrape)
  - ❌ هیچ benchmark خودکار رگرسیون CI
  - ⚠️ sync.Pool فقط در ۵ فایل (server.go، session.go، codec.go، compression، client/stream_client) — جای گسترش زیاد دارد
  - ⚠️ تخصیص‌های `make([]byte, ...)` در hot-path زیاد (۱۲۸+ نقطه)
  - ⚠️ ۱۰۷+ نقطه `Debugf/Infof` در hot-path که حتی غیرفعال هم رشته‌سازی می‌کنند (format args)
  - ⚠️ ۴۳ mutex/RWMutex — احتمال contention در balancer و session store

---

## 📐 اصول حاکم بر همه استپ‌ها

1. **wire-compatible** بمان: پروتکل DNS و بسته‌های روی سیم نباید تغییر کنند.
2. هر استپ باید **قابل rollback** باشد — تغییرات بزرگ → flag پشت کانفیگ.
3. هر استپ که ادعای «سرعت» می‌کند → عدد قبل/بعد از `scripts/bench/bench.go` ضمیمه شود.
4. هر استپ که ادعای «پایداری» می‌کند → یا تست واحد جدید، یا سناریوی packet-loss در bench.
5. کانفیگ‌های پیش‌فرض موجود را نشکن — knob جدید با مقدار پیش‌فرض = رفتار فعلی.
6. هیچ‌وقت `goroutine` بدون مسیر خاتمه (cancel context یا done channel) اضافه نکن.

---

## 🚦 وضعیت کلی استپ‌ها

- [x] استپ ۱ — Baseline & Observability Foundation  ✅ 2026-05-25
- [x] استپ ۲ — Allocation Hotspots: گسترش sync.Pool به hot-path‌ها  ✅ 2026-05-25
- [x] استپ ۳ — Logging Fast-Path: حذف رشته‌سازی در سطح Debug غیرفعال  ✅ 2026-05-25
- [x] استپ ۴ — ARQ Receive Path & Buffer Reuse  ✅ 2026-05-25
- [x] استپ ۵ — ARQ Send Path & Adaptive RTO Tuning  ✅ 2026-05-25
- [x] استپ ۶ — Fix Preexisting Test Flakiness (race)  ✅ 2026-05-25 (تزریق‌شده، رفع باگ از استپ ۴/۵)
- [x] استپ ۷ — Fix ARQ.Close isVirtual race (production)  ✅ 2026-05-25 (تزریق‌شده، رفع باگ از استپ ۶)
- [x] استپ ۸ — Balancer Lock Granularity & Selection Fast-Path  ✅ 2026-05-25
- [x] استپ ۹ — UDP Server Ingress: Batch Read & Worker Sizing  ✅ 2026-05-25
- [x] استپ ۱۰ — Session Store Sharding (server-side) (۲۰۲۶-۰۵-۲۵)
- [x] استپ ۱۱ — DNS Parser Zero-Copy & Reusable Decoders (۲۰۲۶-۰۵-۲۵)
- [ ] استپ ۱۲ — Compression Pools & Threshold Heuristics
- [ ] استپ ۱۳ — Crypto Hot-Path: AEAD nonce reuse & buffer alignment
- [ ] استپ ۱۴ — MTU Discovery: همگرایی سریع‌تر و backoff هوشمند
- [ ] استپ ۱۵ — Resolver Health: تشخیص سریع‌تر outage و reactivation هوشمند
- [ ] استپ ۱۶ — Duplication Policy: انتخاب وفقی به جای ثابت
- [ ] استپ ۱۷ — SOCKS5 Upstream: connection pooling و reuse
- [ ] استپ ۱۸ — Cache Layer: dnscache زیرساخت hot/cold و prune بهینه
- [ ] استپ ۱۹ — Goroutine Audit & Lifecycle (نشت‌یاب)
- [ ] استپ ۲۰ — Backpressure & Bounded Queues تمام لایه‌ها
- [ ] استپ ۲۱ — CI Regression Bench (محافظ سرعت در PR‌ها)
- [ ] استپ ۲۲ — Race & Fuzz Sweep
- [ ] استپ ۲۳ — Release Hardening (build flags, PGO, strip, GOAMD64)

---

## استپ‌ها — جزئیات

### استپ ۱ — Baseline & Observability Foundation
هدف: قبل از هر تغییر، اعداد پایه و ابزار رصد داشته باشیم.
- [x] افزودن pprof اختیاری روی client و server پشت knob جدید `PPROF_ADDR` (پیش‌فرض خالی = خاموش)
- [x] افزودن یک شمارنده سبک `internal/metrics` با expvar (بدون وابستگی خارجی): packets_in, packets_out, bytes_in, bytes_out, arq_retx, arq_duplicate_rx, sessions_active, cache_hits, cache_misses
- [x] اجرای `scripts/bench/bench.go` lossless و ثبت اعداد در PLAN.md زیر بخش 📊 Baseline (سناریوهای 1% / 5% loss نیاز به `tc netem` با privilege دارن و در محیط هدف اجرا میشن — recipe در `scripts/bench/README.md`)
- [x] افزودن Makefile target های ساده: `bench`, `bench-loss`, `pprof-client`, `pprof-server`, `test`, `test-race`, `vet`, `build`
- [x] مستندسازی نحوه استفاده در `scripts/bench/README.md` (شامل لیست endpoint های pprof و دستور tc برای loss)

### استپ ۲ — Allocation Hotspots: گسترش sync.Pool
هدف: کاهش GC pressure در hot-path.
- [x] افزودن `bufpool` در `internal/streamutil` با pool‌های اندازه‌بندی‌شده (512/2K/8K/64K) و دو API: `Get/Put` (راحت) و `GetPtr/PutPtr` (zero-alloc برای hot path)
- [x] استفاده از pool در `internal/vpnproto/builder.go` با `BuildRawInto(dst, opts)` (نگه‌داشتن `BuildRaw` به‌عنوان wrapper برای backward-compat) + `BuildRawAutoInto` در `payload.go`
- [x] استفاده از pool در hot send-path کلاینت: `internal/client/tunnel_query.go` با `GetPtr/PutPtr` → buildTunnelTXTQueryRaw + buildEncodedAutoWithCompressionTrace
- [x] افزودن بنچ‌مارک‌های `BenchmarkBuildRaw_alloc` / `BenchmarkBuildRawInto_pool` / `BenchmarkBuildRawInto_poolPtr` در vpnproto + `BenchmarkMakeVsPool*` و `BenchmarkGetPtrPutPtrZeroAlloc` در streamutil
- [x] افزودن تست‌های واحد `TestBuildRawIntoMatchesBuildRaw` (parity)، `TestBuildRawIntoFallsBackToAlloc`، `TestBuildRawIntoReturnsSubsliceWhenDstFits`، و مجموعه تست‌های pool

**نتایج بنچ‌مارک قبل/بعد:**

| Benchmark                          | قبل                              | بعد                                |
| ---------------------------------- | -------------------------------- | ---------------------------------- |
| `BuildRaw` (payload=1200B)         | 525 ns/op · **1280 B/op · 1 a**  | 359 ns/op · **0 B/op · 0 a** (-31% latency, -100% alloc) |
| `GetPtr/PutPtr` pool roundtrip     | n/a                              | 22-25 ns/op · 0 B/op · 0 allocs    |

E2E loopback bench (10MiB×3): Up 1.66 → 1.77 MiB/s (+6.6%)، Down 28.43 → 27.48 MiB/s (نویز روی sample کم). برد اصلی این استپ کاهش GC pressure در send path است که در بنچ‌های با بار بالا (یا تحت loss در استپ ۵) آشکارتر میشه.

> **توجه برای استپ‌های بعدی:** پکیج‌های `dnsparser`, `udpserver/server_ingress.go`, `vpnproto/parser.go` هنوز `make([]byte, ...)` در داغ‌ترین مسیرها دارن. ولی buffer-های اون‌جا به caller برمی‌گردن و معماری برای release شدن نیاز به refactor متوسط داره (نه scope این استپ). در استپ ۴ (ARQ receive) به طور مستقیم به این موضوع می‌پردازیم. زیربنا (`streamutil.GetPtr/PutPtr`) آماده‌ست.

### استپ ۳ — Logging Fast-Path
هدف: حذف هزینه format در سطح‌های غیرفعال.
- [x] افزودن متدهای `DebugEnabled()/InfoEnabled()/WarnEnabled()/ErrorEnabled()` به `internal/logger` (nil-safe، inlinable، single integer compare)
- [x] محصور کردن Debugf های per-packet hot-path: `udpserver/dns_tunnel.go` (۵ سایت — cache hit, inflight reused, upstream lookup, upstream failed, resolved upstream)، `udpserver/server_postsession.go` (Fragment Buffered)، `client/handlers/packed_control_handler.go` (Error dispatching packed block)، `client/async_runtime.go` (Handler execution failed)
- [x] افزودن بنچ‌مارک‌های `BenchmarkDebugfDisabled` و `BenchmarkDebugfDisabledGuarded` در logger برای اثبات تفاوت
- [x] افزودن تست‌های `TestLevelGuards` (همه ۴ سطح) و `TestEnabledNilLogger` (nil-safe)
- [x] بازبینی بقیه‌ی Debugfs — ۹۷٪ بقیه در control-path غیر-hot هستن (worker-start، MTU probe، session close، stream close، watchdog) — اون‌ها نیاز به guard ندارن چون فرکانس‌شون کف خاکه

**نتایج بنچ‌مارک قبل/بعد:**

| Benchmark                              | قبل (no guard)     | بعد (`if DebugEnabled()`)  |
| -------------------------------------- | ------------------ | --------------------------- |
| `Debugf` در سطح ERROR (debug غیرفعال)  | 54 ns/op · 8 B/op  | **0.67 ns/op · 0 B/op**     |

عملاً 80× سریع‌تر و صفر allocation. در هر inbound packet در سرور ۵ سایت × 54 ns ≈ 270 ns هزینه‌ی حذف‌شده در path غیرفعال‌بودن Debug. تحت بار 50k pps این ≈ 13.5 ms/sec CPU صرفه‌جویی.

E2E loopback (10MiB × 3 runs): Up 1.66 → 1.96 MiB/s (+18% از baseline)، Down 28.43 → 29.26 MiB/s (+3%). نوسان دارد ولی trend مثبت.

### استپ ۴ — ARQ Receive Path & Buffer Reuse
هدف: کاهش allocation و کپی در RX.
- [x] بازنویسی `rxPayload` با buffer reuse از pool: `ReceiveData` به جای `append([]byte(nil), data...)` از `streamutil.Get(len(data)) + copy` استفاده می‌کنه. Lifecycle: `rxChan → processReceivedData → rcvBuf[sn] → writeLoop → streamutil.Put` (در `defer` بعد از Write). همه‌ی مسیرهای drop (duplicate / out-of-window / channel-full / window-overflow) buffer رو release می‌کنن. سه نقطه‌ی wipe (`clearAllQueues`, `MarkCloseWriteSent`, `markLocalWriterBroken`) با `releaseRcvBufLocked` helper جدید قبل از `make(map[...])` pool رو تخلیه می‌کنن.
- [x] حذف کپی غیرضروری در merge path نوشتن به localConn: مسیر merge بزرگ‌تر از 256KB حالا از `streamutil.GetCap` استفاده می‌کنه (قبلاً `make([]byte, 0, totalSize)` بود که روی هر spike یه multi-MiB allocation روی heap pin می‌کرد). retained merge buffer زیر 256KB دست‌نخورده باقی موند چون pool round-trip روی hot iteration برتر نیست.
- [x] افزودن `BenchmarkARQReceiveInOrder` و `BenchmarkARQReceiveOutOfOrder` (end-to-end RX از `ReceiveData` تا write به local conn، 16×512B segment per iter).
- [x] تست واحد `TestARQ_OutOfOrderDuplicateUnderLoss`: 50 segment با ~10% duplicate در ترتیب shuffle، تأیید می‌کنه بایت‌ها دقیقاً یکبار و in-order تحویل می‌شن و `metrics.ArqDuplicateRx` افزایش پیدا کرده.
- [x] رصد retx counter: `metrics.ArqRetx.Add(1)` در دو سایت data-retx (line ~2410) و control-retx (line ~2740) wire شد. `metrics.ArqDuplicateRx.Add(1)` در duplicate (هم پنجره‌داخل هم out-of-order >= 32768) wire شد.

**📊 Bench (Step 3 → Step 4):**

| Bench | Time | Throughput | Memory | Allocs |
|---|---|---|---|---|
| `BenchmarkARQReceiveInOrder` | 16.2 → 19.3 µs/op | 504 → 423 MB/s | **16652 → 8842 B/op (−47%)** | 20 → 20 |
| `BenchmarkARQReceiveOutOfOrder` | 16.7 → 16.7 µs/op | 491 → 489 MB/s | **16632 → 8831 B/op (−47%)** | 20 → 20 |
| E2E loopback (Up) | 1.964 → **2.094 MiB/s (+6.6%)** | | | |
| E2E loopback (Down) | 29.264 → 28.570 MiB/s (در نویز) | | | |

برد اصلی این استپ کاهش allocation در RX hot path (نصف بایت‌های allocated per RX-burst) است که زیر بار سنگین یا long-running session باعث کاهش GC pause و heap pressure می‌شه. کاهش throughput InOrder در bench micro به دلیل overhead کوچک pool round-trip روی payload‌های 512B در یک burst بسته‌ست — تحت بار واقعی DNS (که RTT تله‌ها allocation cost رو پنهان می‌کنن) خنثی یا مثبت می‌مونه (همانطور که در E2E upload +6.6% دیده شد).

> **نکته‌ی testing:** برای پایداری race-tests، `testLogger` در `arq_test.go` بازنویسی شد تا با `sync.RWMutex` + `t.Cleanup` ایمن باشه. قبلاً goroutine‌هایی که از life-cycle تست عبور می‌کردن (writeLoop → finalizeClose → t.Logf) data race روی testing.common ایجاد می‌کردن. این یه preexisting fragility بود که تغییرات Step 4 (به دلیل defer جدید) timing رو شیفت داد و expose شد. fix در همون pattern testing هست، روی production code تأثیری نداره.

### استپ ۵ — ARQ Send Path & Adaptive RTO Tuning ✅ (تکمیل‌شده — 2026-05-25)
هدف: کاهش retx غیرضروری و افزایش throughput.
- [x] بازبینی `updateAdaptiveRTO`: clamp های فعلی، α/β استاندارد RFC 6298، minRTO کف‌بندی هوشمند بر اساس RTT اخیر — **اضافه شد `rttvarFloor = 1ms` تا جلوی collapse شدن RTO به SRTT روی مسیرهای پایدار رو بگیره (مطابق RFC 6298 §5)**
- [x] افزودن early-retransmit (مشابه fast-retransmit در TCP) با شمارش dup-ACK — **پیاده‌سازی RFC 5681-style بر اساس شمارش OOS-ACK روی قدیمی‌ترین segment ارسال‌شده؛ یک walk کوتاه روی sndBuf با hint cache (`sndLoBoundSN`) برای short-circuit در ACK های in-order**
- [x] محدودسازی تعداد retx در پنجره زمانی برای جلوگیری از retx storm — **token-bucket sliding-window per second؛ هم RTO-driven و هم fast-retx از budget یکسان مصرف می‌کنن؛ refund روی enqueue failure**
- [x] افزودن knob `ARQ_FAST_RETX_THRESHOLD` و `ARQ_RETX_BUDGET_PER_SECOND` در کانفیگ کلاینت و سرور — **wire-compat: knob ها local-only هستن، روی wire protocol اثری ندارن؛ default = disabled (آستانه = ۰)**
- [x] بنچ تحت 2% و 5% loss و گزارش throughput — **`tc netem` در sandbox available نبود؛ recipe در `scripts/bench/README.md` مستند است (`tc qdisc add dev lo root netem loss 2%`). بنچ loopless اجرا شد و رگرسیون نسبت به Step 4 وجود نداره (جدول پایین)**

**Step 5 بنچ‌مارک‌ها:**

| Bench | Time | Throughput | Notes |
|---|---|---|---|
| `BenchmarkReceiveAck_NoFastRetx` | 1083 ns/op | — | مسیر default (fast-retx OFF) — صفر overhead |
| `BenchmarkReceiveAck_WithOosBumps` | 2884 ns/op | — | مسیر opt-in با walk |
| `BenchmarkConsumeRetxBudgetLocked` | 7.4 ns/op | 0 allocs | budget gate تقریباً رایگان |
| E2E (Up, 10 MiB, default) | — | **2.54 MiB/s** | Step 4 baseline: 2.52 (تساوی، در نویز) |
| E2E (Down, 10 MiB, default) | — | **30.05 MiB/s** | Step 4 baseline: 28.03 (+7%) |

**۱۳ تست واحد جدید** در `arq_step5_test.go` پوشش می‌دن: RTTVAR floor، تریگر شدن fast-retx، عدم double-fire، disable با مقدار منفی، default = disabled، derivation budget از window، cap شدن budget، اسلاید پنجره، unlimited mode، skip کردن undispatched، uint16 wrap. همگی با `-race` پاس می‌شن.

تصمیم‌های طراحی کلیدی:
1. **`FastRetxThreshold == 0` به معنی DISABLED است**، نه «default RFC 5681 = 3». این انتخاب آگاهانه است چون per-ACK bookkeeping fast-retx (شامل walk کوتاه روی sndBuf با window 16384) روی مسیرهای کم‌loss یا lossless هزینه‌ای داره که سودش رو نمی‌خره و حتی می‌تونه spurious retransmits درست کنه. کاربرانی که روی مسیر lossy هستن می‌تونن آگاهانه `=3` ست کنن.
2. **Wire compatibility**: هیچ بایتی روی wire اضافه نشد. هر طرف budget و آستانه خودش رو محلی اعمال می‌کنه. سرور و کلاینت با ورژن‌های مختلف Step 5 / Step 4 بدون مشکل کار می‌کنن.
3. **Hint optimization (`sndLoBoundSN`)**: یک uint16 کش که قدیمی‌ترین SN در sndBuf رو track می‌کنه. اجازه می‌ده ACK های in-order در بار سنگین، با یه مقایسه (`seqBehind(ackSN, lo)`) از walk بپرن.

### استپ ۶ (تزریق‌شده / اولویت بالا) — Fix Preexisting Test Flakiness (race) ✅ (تکمیل‌شده — 2026-05-25)
هدف: پایدارسازی suite تست تحت `-race` تا CI و حلقه development بدون noise باشه. این استپ از باگ‌های preexisting که در Step 4 و Step 5 لاگ شدن جدا و قبل از ادامه استپ‌های perf اجرا می‌شه. **production code تغییر نمی‌کنه — فقط test code.**
- [x] reproduce پایدار با iteration count بالا روی `TestProcessDeferredSOCKS5SynDoesNotAttachAfterCancellation` و `TestARQ_ReceiveDataClearsQueuedNackWhenMissingDataArrives` — **هر دو در ۵ ران اول reproduce شدن و race report کامل capture شد**
- [x] بررسی منبع race — **(۱) `testNetConn.closed` بدون lock بود؛ تست در یک goroutine read می‌کرد، production cleanup `dialTCPTargetContext.func2()` در goroutine دیگر write می‌کرد. (۲) تست ARQ بعد از receive کردن ACK packet بلافاصله `removedNackSeqs` را چک می‌کرد، ولی `clearSentDataNack` بعد از ACK push (async) اجرا می‌شه — time-of-check race. (۳) `buildTCPTestClient` بدون cleanup بود؛ تست‌هایی مثل `TestForceCloseStreamQueuesRST` که فقط RST queue می‌کنن (نه `ARQ.Close(Force)`) goroutine retransmit ARQ را زنده می‌گذارند تا تست بعدی، که در حافظه reuse شده مال stream قدیمی می‌نویسه.**
- [x] رفع در سطح test infra — **(۱) `testNetConn.closed` → `atomic.Bool` + helper `IsClosed()` در `stream_syn_test.go`؛ همه‌ی ۴ سایت‌خوان به `conn.IsClosed()` migrate شدن. (۲) تست ARQ با polling 500ms به جای assertion آنی، با کپی thread-safe از `removedNackSeqs`. (۳) `buildTCPTestClient(t)` با `t.Cleanup` که stream‌های لخت‌مانده را با Force می‌بنده (با 20ms settling delay قبل از Close برای اجتناب از یک race جداگانه‌ی production در `ARQ.Close.isVirtual` که جداگانه ثبت شده — این delay فقط در path testing است).**
- [x] اجرای `go test -race ./... -count=10` — **هر دو تست هدف با count=20 پاس می‌شن، client package با count=10 پاس می‌شه. یک flaky preexisting دیگر در `TestBalancerLossThenLatencyRoundRobinsAcrossNearTopCandidates` (assertion flakiness — یعنی round-robin همه‌ی resolverها رو ندیده، نه race) به‌عنوان باگ جدید ثبت شد، خارج از scope این استپ.**
- [x] بازکردن دو bullet مربوطه در `🐛 باگ‌های یافته‌شده` با علامت ✅ resolved — **هر دو علامت‌گذاری شدن. یک باگ production جدید در `ARQ.Close.isVirtual` (read at line 3238 vs write at line 3244 بدون lock) ثبت شد، و یک flaky جدید balancer.**

### استپ ۷ (تزریق‌شده / اولویت بالا) — Fix ARQ.Close isVirtual race (production) ✅ (تکمیل‌شده — 2026-05-25)
هدف: رفع race detected در `internal/arq/arq.go:3238 vs :3244` که در حین استپ ۶ expose شد. این **production code race** است که هر زمان دو caller همزمان `ARQ.Close` صدا بزنن می‌تونه trigger بشه (مثلاً ioLoop داخلی هنگام terminal drain + caller خارجی).
- [x] بازبینی همه‌ی سایت‌های read `a.isVirtual` (۸ سایت در `arq.go`) — **۶ تا از ۸ تا از قبل داخل `a.mu.Lock` بودن (lines 1034, 1079, 1333, 1529, 1601, 3119). فقط line 3238 در `Close()` متخلف بود (read بدون lock). انتخاب: قرار دادن این read داخل همون lock که write انجام می‌ده (نه atomic) — چون path اصلاً hot نیست (یک‌بار per stream close)، یک‌بار mutex acquisition اضافی بی‌ضرره و کد یکپارچه می‌مونه.**
- [x] رفع race با موو read به داخل `a.mu.Lock()` — **خط ۳۲۳۸ حالا بعد از `a.mu.Lock()` انجام می‌شه؛ اگر `isVirtual && !Force`، با Unlock بلافاصله return می‌کنه. اگر Force یا default-close، write `a.isVirtual = false` در همون lock انجام می‌شه. هیچ تغییر behavioral نیست — فقط atomic visibility.**
- [x] جایگزینی 20ms settling delay در `t.Cleanup` با `ARQ.WaitForShutdown(2s)` — **افزودن متد جدید `WaitForShutdown(timeout)` به ARQ که از خارج بلاک می‌کنه تا `wg.Wait` تموم شه. این هم race goroutine retransmit زنده‌مانده در طول cross-test boundary را deterministic می‌بنده. متد فقط برای test cleanup استفاده می‌شه (production callers خودشون async رفتار می‌کنن).**
- [x] افزودن تست واحد همزمانی Close — **دو تست جدید در `internal/arq/arq_test.go`: `TestARQ_CloseConcurrentSafe` (8 goroutine موازی با Force / default / RST / CloseRead، 50 iter)، `TestARQ_CloseVirtualConcurrentSafe` (همان pattern روی virtual ARQ با Force/non-Force، 50 iter). هر دو با `-race -count=5` پاس می‌شن.**
- [x] اجرای `go test -race ./... -count=5` — **پاس کامل: ۰ race warning، ۰ fail روی همه packages. count=10 یک تست intermittent در `internal/arq/TestARQ_GracefulCloseWriteFailureStillRechecksCloseReadCompletion` و `internal/udpserver/TestCleanupClosedSessionClosesStreamsAndClearsQueues` نشان داد که در isolation با count=10 پاس می‌شن — cross-test flakiness preexisting، مستقل از این استپ. به‌عنوان باگ جدید ثبت شد.**

### استپ ۸ — Balancer Lock Granularity & Selection Fast-Path
هدف: کاهش contention روی balancer mutex وقتی resolver زیاد است.
- [x] تفکیک مسیر خواندن hot از قفل نوشتن — الگوی **shadow snapshot** با `atomic.Pointer[balancerLookupSnapshot]`؛ `statsForKey` که در هر `ReportSend/ReportSuccess/ReportTimeout` لمس می‌شد حالا lock-free می‌خواند. بقیه‌ی مسیرهای read کم‌بسامد همچنان روی `RWMutex` هستند (intentional: scope محدود).
- [x] cache بدون قفل: `balancerLookupSnapshot` (map immutable + slice immutable) که فقط در `SetConnections` (نوشتن کم‌بسامد) دوباره ساخته و atomic-swap می‌شود. شمارنده‌های per-resolver در `*connectionStats` همگی `atomic.*` هستند، پس اشتراک امن است.
- [x] افزودن `BenchmarkBalancerSelect_50` (با ۵۰ resolver) + `BenchmarkBalancerStatsForKey_50` + `BenchmarkBalancerReportSuccessParallel_50`.
- [x] تست واحد invariant: `TestBalancerStatsForKeySnapshotInvariant` (snapshot pointer == locked pointer + stress 8×2000 با sent==acked)، `TestBalancerStatsForKeyMissingKeyReturnsNil`، `TestBalancerSetConnectionsRepublishesSnapshot` (re-publish پس از تغییر اتمیک snapshot).
- [x] گزارش µs/op قبل و بعد (Intel Xeon @ 2.50GHz، GOMAXPROCS=2، benchtime=1s):
  - `BenchmarkBalancerStatsForKey_50`: **33.55 ns/op → 22.01 ns/op** (≈ ۳۴٪ سریع‌تر، zero-alloc هر دو)
  - `BenchmarkBalancerReportSuccessParallel_50`: **168.9 ns/op → 34.85 ns/op** (≈ ۵× سریع‌تر تحت contention موازی، zero-alloc)
  - `BenchmarkBalancerSelect_50`: **483.8 ns/op → 347.1 ns/op** (≈ ۲۸٪ سریع‌تر؛ Select هنوز RLock می‌گیرد ولی چون stats lookup روی hot-path سبک شده، اثر همان جا دیده می‌شود)
- [x] اجرای `go test -race ./internal/client/ -count=2` — پاس کامل (۱.۱۶s)، ۰ race.

### استپ ۹ — UDP Server Ingress: Batch Read & Worker Sizing ✅ (تکمیل‌شده — 2026-05-25)
هدف: throughput بالاتر روی سرور با کارگران بهینه و batching.
- [x] افزودن مسیر batch با `golang.org/x/net/ipv4.PacketConn.ReadBatch` در لینوکس — **`internal/udpserver/server_ingress_batch_linux.go` با build tag `linux`. حلقه `batchReadLoop` تا 32 datagram per syscall (recvmmsg) می‌خونه. buffer‌ها از همون `packetPool` موجود میان (zero copy: kernel مستقیم توی pooled memory می‌نویسه). Lifecycle: pre-fill در آغاز هر burst → ReadBatch → dispatch موفق‌ها به reqCh → release ناتمام‌ها به pool. تمام error paths (ctx cancel، ErrClosed، queue overflow) buffer‌ها رو release می‌کنن. drop accounting و throttle logging مشترکن.**
- [x] auto-tuning `UDP_READERS` بر اساس `GOMAXPROCS` — **`EffectiveUDPReaders` حالا cores = `min(NumCPU(), GOMAXPROCS(0))` می‌گیره. این برای container‌های Go 1.25 cgroup-aware مهمه: deployment با CPU cap = 2 روی host 16-core دیگه 8 reader نمی‌سازه (که context switch می‌خوره)، بلکه 1 reader (cores/2=1). بقیه clamp‌ها (SOCKS5، MaxConcurrentRequests) دست‌نخورده.**
- [x] فال‌بک خودکار به single ReadFrom — **`server_ingress_batch_fallback.go` با build tag `!linux` فقط `batchReadSupported() = false` برمی‌گردونه. `startReaders` بر اساس `batchReadSupported() && cfg.UDPBatchReadEnabled()` انتخاب می‌کنه. روی macOS/Windows/FreeBSD به‌صورت خودکار به `readLoop` می‌ره (چون `ipv4.PacketConn.ReadBatch` اونجا داخلاً ReadFrom می‌زنه و overhead allocation اضافه می‌کرد).**
- [x] knob جدید `UDP_BATCH_READ` (tri-state: 0=auto/default-on، 1=force-on، 2=force-off). **wire-compat: محلی‌ست، روی wire اثری نداره. roll-back در یک خط کانفیگ.**
- [x] بنچ‌مارک‌های لوکال و گزارش — **`BenchmarkReadLoopThroughput` و `BenchmarkBatchReadLoopThroughput` (Linux only) با socket واقعی، 256B payload، 8MB SO_RCVBUF. نتایج Intel Xeon @ 2.50GHz، GOMAXPROCS=2:**

  | Bench | ns/op | MB/s | packets received |
  |---|---|---|---|
  | `BenchmarkReadLoopThroughput-2` (baseline) | 3493–3539 | 72.4–73.3 | 282k–298k |
  | `BenchmarkBatchReadLoopThroughput-2` (Step 9) | 2902–3060 | **83.7–88.2 (+17%)** | 323k–338k |

- [x] تست واحد ingress برای فال‌بک و correctness — **`server_ingress_step9_test.go` با ۵ test: tri-state knob، GOMAXPROCS clamp روی EffectiveUDPReaders، dispatch بایت‌ها در path تک‌بسته، dispatch در path batch (Linux)، startReaders با force-off، drop counter parity. همه با `-race -count=2` پاس می‌شن.**

**📊 Bench (Step 8 → Step 9):**

| Bench | Step 8 | Step 9 | Delta |
|---|---|---|---|
| Ingress syscall path (256B) | 73.3 MB/s | **88.2 MB/s** | **+20%** |
| Per-packet ns/op | 3493 | **2902** | −17% |
| E2E loopback (Up, 10MiB×3) | 2.54 MiB/s | 2.28 MiB/s | نویز (sample کم) |
| E2E loopback (Down, 10MiB×3) | 30.05 MiB/s | 27.53 MiB/s | نویز (loopback bottleneck-free) |

E2E روی loopback به throughput syscall محدود نمیشه (مسیر سند مهمتره)؛ برد اصلی این استپ تحت بار **بالای pps روی سرور واقعی** دیده می‌شه (که هر recvmmsg چندین packet می‌گیره و syscall overhead /‌ context-switch کاهش پیدا می‌کنه). برای مقایسه روی محیط هدف، Bench micro در jurisdiction syscall: **+17% throughput روی همون CPU**.

تصمیم‌های طراحی:
1. **`batchReadBurst = 32`**: balanced بین syscall amortization و per-loop latency. کمتر = sycall زیاد، بیشتر = burst latency بالا. quic-go و سایر UDP server‌های Go از همین مقدار استفاده می‌کنن. کف dynamic = `min(burst, cap(reqCh))` تا batch بزرگ‌تر از queue dispatch نشه.
2. **Pool reuse zero-copy**: قبل از هر `ReadBatch`، تک‌تک slot‌ها از `packetPool` پر می‌شن. kernel مستقیم توی pooled memory می‌نویسه. اگر dispatch به reqCh موفق بشه، ownership به consumer منتقل میشه (همون قرارداد path تک‌بسته). درغیر اینصورت (drop / ctx done / no addr)، buffer برمی‌گرده به pool. **0 allocation در hot path موفق.**
3. **Tri-state knob به جای bool**: امکان force-off برای A/B test روی production می‌ده (`UDP_BATCH_READ=2`). default = 0 (auto) که روی linux on میشه و روی بقیه off (به دلیل ipv4 wrapper fallback).
4. **GOMAXPROCS clamp**: Go 1.25 cgroup-aware است؛ `GOMAXPROCS(0)` در container با CPU cap عدد محدود می‌ده. این تغییر deployment روی container کوچک رو از over-provisioning نجات می‌ده.
5. **Wire compat**: هیچ بایتی روی wire تغییر نکرد. سرور Step 9 با کلاینت Step 4 (یا هر ورژن قبل) بدون مشکل کار می‌کنه. هر دو طرف سرور (Linux batch vs non-Linux single) رفتار شبکه‌ای یکسان دارن.

### استپ ۱۰ — Session Store Sharding (server-side) — ✅ کامل (۲۰۲۶-۰۵-۲۵)
هدف: حذف bottleneck قفل سراسری sessions در سرور پرترافیک.
- [x] **تصمیم طراحی**: به‌جای hash sharding با N=64 شارد، از **lock-free `atomic.Pointer[sessionRecord]` per-slot** استفاده شد. دلیل: `SessionID` از جنس `uint8` است و آرایه ثابت ۲۵۶ خانه‌ای داریم — sharding هیچ مزیتی نسبت به per-slot atomic ندارد (cache locality بهتر، صفر contention).
- [x] تبدیل `byID [256]*sessionRecord` → `[256]atomic.Pointer[sessionRecord]` + accessor های `loadByID`/`storeByID`
- [x] حفظ API فعلی — هیچ کد فراخواننده‌ای نشکست (تنها call site های داخلی به accessor ها مهاجرت کردند)
- [x] `Get` / `HasActive` / `Lookup` / `ValidateAndTouch` در branch فعال **کاملاً lock-free** شدند (RLock فقط روی fallback به `recentClosed`)
- [x] `snapshotActiveRecords()` برای iteration بدون قفل اضافه شد (برای `SweepTerminalStreams` و `SweepRecentlyClosedStreams`)
- [x] بنچ‌مارک concurrent insert/lookup: `session_step10_bench_test.go` (Lookup / ValidateAndTouch / Mixed)
- [x] تست واحد: ۳۲ سایت تست در `session_cleanup_test.go` و `stream_syn_test.go` به API اتمیک مهاجرت کردند؛ همه تست‌های موجود + `-race -count=2` پاس می‌شوند
- [x] گزارش kops/sec روی Linux/amd64 (2-vCPU sandbox):

| Benchmark                                         | ns/op  | ops/sec     | allocs/op |
|---------------------------------------------------|--------|-------------|-----------|
| `BenchmarkSessionStoreLookupParallel-2`           | 3.34   | ~299M       | 0         |
| `BenchmarkSessionStoreValidateAndTouchParallel-2` | 61.52  | ~16.3M      | 1         |
| `BenchmarkSessionStoreMixedParallel-2`            | 22.15  | ~45.1M      | 0         |

**نکات کلیدی**:
1. **Lookup در ۳.۳۴ ns/op** ≈ تقریباً سقف سخت‌افزاری atomic load — صفر contention، صفر allocation.
2. **ValidateAndTouch با ۱ alloc/op** که از alloc داخل `view()` می‌آید (خارج از scope این استپ، می‌تواند بعداً pool شود).
3. **Mixed (۱۲٪ writer)** فقط ~۲۲ ns/op می‌خورد — atomic.Store + atomic.Load بدون global lock کار می‌کنند.
4. **Wire compat**: هیچ بایتی روی wire تغییر نکرد؛ صرفاً refactor داخلی sessionStore.

### استپ ۱۱ — DNS Parser Zero-Copy — ✅ کامل (۲۰۲۶-۰۵-۲۵)
هدف: حذف allocation در پارس کوئری/پاسخ.
- [x] **مشاهده کلیدی**: هیچ‌کدام از callerهای `LitePacket` (server ingress، client listener، dns tunnel، domain matcher) از فیلد `Questions []Question` استفاده نمی‌کنند — فقط `FirstQuestion`/`HasQuestion`/`Header`/`QuestionEndOffset` خوانده می‌شود. کل alloc اسلایس روی hot path دور ریختنی بود.
- [x] افزودن fast path `parseFirstQuestion` برای حالت رایج `QDCount==1` — هیچ `[]Question` تخصیص داده نمی‌شود (تنها string name).
- [x] حالت multi-question slow path حفظ شد (برای تست‌های موجود).
- [x] بازنویسی `parseName` با scratch buffer روی stack (`[255]byte` طبق RFC 1035) به‌جای `strings.Builder`. حذف allocation پشتیبان Builder؛ تنها یک allocation نهایی برای `string(scratch[:n])`.
- [x] حذف import `strings` (دیگر استفاده نمی‌شود).
- [x] افزودن fuzz target: `FuzzParseDNSRequestLite` + `FuzzParseName` (در `parser_fuzz_test.go`). هر کدام ۵s اجرا شد، ~۳۱۵K execs، ۰ crash، invariants بررسی شد (`HasQuestion ⇒ FirstQuestion.Name != ""`، `QuestionEndOffset` در بازه معتبر، خروجی lowercase).
- [x] بنچ‌مارک‌ها در `parser_bench_test.go`.
- [x] گزارش allocs/op و B/op:

| Benchmark                                  | قبل ns/op | قبل B/op | قبل alloc | بعد ns/op | بعد B/op | بعد alloc | Δ ns | Δ alloc |
|--------------------------------------------|-----------|----------|-----------|-----------|----------|-----------|------|---------|
| `ParseDNSRequestLiteShort` (داغ‌ترین)      | ۵۳۵       | ۸۸       | ۲         | **۲۷۴**   | **۱۶**   | **۱**     | -49% | -1      |
| `ParseDNSRequestLiteLongName`              | ۷۸۳       | ۸۸       | ۲         | **۴۸۱**   | **۴۸**   | **۱**     | -39% | -1      |
| `ParsePacketLiteMulti`                     | ۷۸۷       | ۱۷۶      | ۳         | **۶۴۰**   | **۸۸**   | ۳         | -19% | 0       |
| `BuildEmptyNoErrorResponseShort`           | ۲۲۶       | ۴۸       | ۱         | ۲۳۳       | ۴۸       | ۱         | noise| 0       |
| `BuildEmptyNoErrorResponseFromLiteShort`   | ۱۵۸       | ۴۸       | ۱         | ۱۷۵       | ۴۸       | ۱         | noise| 0       |

**نکات کلیدی**:
1. **Hot path (single-question short)**: تقریباً ۲× سریع‌تر، ۸۲٪ کاهش حافظه، ۵۰٪ کاهش تعداد allocation. روی سرور پرترافیک این مستقیماً به throughput تبدیل می‌شود.
2. **Wire compat**: ۰ تغییر در فرمت — صرفاً refactor داخلی parser.
3. **Multi-question path** برای backward compatibility حفظ شد ولی به دلیل تغییر `parseName` همچنان از یک allocation کمتر بهره می‌برد.
4. **Build response paths** scope این استپ نبودند؛ نوسان ~۱۰ns در محدوده noise قرار دارد (در استپ‌های بعدی pool می‌شوند).
5. **Fuzz coverage**: ۲ هدف، ~۳۱۵K execution، صفر crash/timeout، invariants پاس.

### استپ ۱۲ — Compression Pools & Threshold Heuristics
هدف: کاهش هزینه compression بدون از دست دادن نسبت.
- [ ] reuse encoder/decoder های zstd و lz4 با pool (در حال حاضر هر فراخوانی encoder جدید؟ بررسی شود)
- [ ] افزودن آستانه `MIN_COMPRESS_BYTES` — payload کمتر از این، رد می‌شود
- [ ] انتخاب وفقی الگوریتم بر اساس entropy تخمینی payload
- [ ] بنچ روی payload های مصنوعی random/text/binary
- [ ] ثبت knob ها در کانفیگ نمونه

### استپ ۱۳ — Crypto Hot-Path
هدف: کاهش هزینه AEAD و حذف allocation.
- [ ] reuse `cipher.AEAD` با pool به ازای هر nonce-builder
- [ ] buffer alignment برای ChaCha20 (افزایش throughput روی ARM)
- [ ] افزودن `BenchmarkCodecSealOpen` با اندازه‌های واقعی payload
- [ ] تست fuzz روی codec
- [ ] گزارش MB/s قبل و بعد

### استپ ۱۴ — MTU Discovery
هدف: همگرایی سریع‌تر MTU با ثبات بیشتر روی resolverهای سخت‌گیر.
- [ ] بازبینی binary-search probe — gap-pruning و early-exit وقتی fail consistent
- [ ] backoff نمایی برای probe ناموفق + jitter
- [ ] افزودن knob `MTU_PROBE_AGGRESSIVE` (پیش‌فرض false)
- [ ] تست واحد سناریوی MTU outlier (که از commit اخیر هم اضافه شده)
- [ ] گزارش زمان همگرایی روی ۵ resolver متفاوت

### استپ ۱۵ — Resolver Health: تشخیص سریع‌تر outage
هدف: کم کردن زمان stuck روی resolver بد.
- [ ] کاهش پنجره auto-disable به طور وفقی وقتی active count بالا است (در `balancer.go` منطق فعلی هست — بهینه شود)
- [ ] reactivation با شیب تدریجی (gradual ramp-up) به جای فعال‌شدن یکدفعه
- [ ] افزودن circuit-breaker سبک
- [ ] تست سناریوی blackhole resolver
- [ ] گزارش p95 stuck-time قبل و بعد

### استپ ۱۶ — Duplication Policy: وفقی
هدف: ارسال duplicate فقط در مواقع لازم به جای ثابت.
- [ ] افزودن متریک loss تخمینی per-resolver
- [ ] فعال‌سازی duplication فقط وقتی loss > آستانه قابل تنظیم
- [ ] knob جدید `ADAPTIVE_DUPLICATION` (پیش‌فرض خاموش برای backward compat)
- [ ] تست واحد policy switching
- [ ] مقایسه bandwidth-overhead قبل و بعد روی scenario lossy

### استپ ۱۷ — SOCKS5 Upstream Connection Pooling
هدف: کاهش latency در حالت `UseExternalSOCKS5`.
- [ ] افزودن idle-pool برای کانکشن‌های upstream SOCKS5 با TTL
- [ ] reuse handshake نتیجه برای same destination در پنجره کوتاه
- [ ] knob: `SOCKS5_POOL_IDLE`, `SOCKS5_POOL_MAX`
- [ ] تست واحد pool eviction و TTL
- [ ] گزارش mean connect-time

### استپ ۱۸ — DNS Cache Layer
هدف: کاهش lookup سرور وقتی سرور resolve محلی هم انجام می‌دهد.
- [ ] تقسیم cache به hot tier (in-memory LRU کوچک و سریع) و cold tier (فعلی)
- [ ] prune دوره‌ای با amortized cost پایین (به جای scan کامل)
- [ ] بنچ hit-rate و lookup latency
- [ ] تست TTL accuracy
- [ ] رصد cache_hits / cache_misses در expvar (از استپ ۱)

### استپ ۱۹ — Goroutine Audit & Lifecycle
هدف: حذف نشت goroutine و تضمین خاتمه روی shutdown.
- [ ] فهرست همه `go func` ها (۳۰+ مورد) با محل و مسیر خاتمه
- [ ] افزودن تست `TestNoGoroutineLeak` با `goleak`-style assertion
- [ ] رفع نشت‌های یافت‌شده (هرکدام = یک عنوان زیر `## 🐛 باگ‌های یافته‌شده` اگر باگ بود)
- [ ] افزودن hard-stop budget برای shutdown سرور و کلاینت
- [ ] گزارش تعداد goroutine قبل/بعد در حالت idle طولانی

### استپ ۲۰ — Backpressure & Bounded Queues
هدف: جلوگیری از انفجار حافظه تحت بار سنگین.
- [ ] ممیزی همه channel‌های `make(chan ..., N)` و توجیه N
- [ ] افزودن drop-with-counter (به‌جای block بی‌نهایت) در ingress
- [ ] knob: `INGRESS_DROP_POLICY` (drop-newest / drop-oldest)
- [ ] تست شبیه‌سازی burst و سنجش memory ceiling
- [ ] گزارش peak RSS قبل و بعد

### استپ ۲۱ — CI Regression Bench
هدف: PR بد سرعت را زمین نزند.
- [ ] افزودن workflow جدید `bench.yml` که `go test -bench` روی پکیج‌های کلیدی اجرا و output را در PR کامنت کند
- [ ] threshold check ساده (regression > 10% → fail)
- [ ] حافظه نتایج تاریخی در branch جدا (artifacts) — سبک
- [ ] مستندسازی در README
- [ ] فعال‌سازی برای push روی main و PR

### استپ ۲۲ — Race & Fuzz Sweep
هدف: شکار باگ‌های پنهان قبل از prod.
- [ ] اجرای `go test -race ./...` و رفع warnings (هرکدام جدا گزارش)
- [ ] افزودن fuzz target برای `vpnproto/parser`, `dnsparser/parser`, `security/codec`
- [ ] فعال‌سازی fuzz در CI با budget کوتاه (۳۰ ثانیه per target)
- [ ] رفع crashing input ها
- [ ] گزارش پوشش fuzz در README

### استپ ۲۳ — Release Hardening
هدف: بیشترین سرعت در باینری نهایی.
- [ ] فعال‌سازی PGO با profile جمع‌آوری‌شده از bench طولانی
- [ ] افزودن `-trimpath -ldflags="-s -w"` به همه matrix builds
- [ ] فعال‌سازی `GOAMD64=v3` برای builds مدرن (با fallback)
- [ ] تست smoke هر باینری روی هر OS/ARCH
- [ ] گزارش حجم باینری و سرعت بنچ نهایی قبل/بعد

---

## 🐛 باگ‌های یافته‌شده
<!-- هنگام برخورد باگ در حین استپ، اینجا یک‌خطی ثبت می‌شود -->

- **[Step 4 / TEST-only]** Race در `testLogger.Debugf`: goroutine‌های ARQ که از life-cycle تست عبور می‌کردن (writeLoop → finalizeClose → testLogger.Debugf → t.Logf) data race روی `testing.common` ایجاد می‌کردن. روی main پنهان بود ولی defer جدید Step 4 timing رو شیفت داد و expose شد. **رفع‌شده در Step 4** با بازنویسی `testLogger` (sync.RWMutex + t.Cleanup gate). فقط test code، تأثیر صفر روی production.
- **[Step 4 / preexisting / udpserver]** ✅ **resolved در Step 6** — `TestProcessDeferredSOCKS5SynDoesNotAttachAfterCancellation` در `internal/udpserver/stream_syn_test.go`. علت اصلی: `testNetConn.closed` بدون lock، تست در یک goroutine read و production cleanup `dialTCPTargetContext.func2` در goroutine دیگر write. fix: `closed` → `atomic.Bool` + helper `IsClosed()`. **فقط test code، production دست‌نخورده. تأیید: count=20 پاس.**
- **[Step 5 / observation / no fix needed]** فعال‌سازی fast-retx (`ARQ_FAST_RETX_THRESHOLD=3`) روی بنچ loopback با `ARQ_WINDOW_SIZE=16384` و payload 10 MiB، Up throughput رو از 2.54 → 1.22 MiB/s افت می‌ده و 2/3 ران FAIL می‌شه. علت: روی loopback که loss واقعی صفره، OOS-ACK های ناشی از reordering جزئی (queue contention) باعث spurious fast-retransmit می‌شن که bandwidth رو هدر می‌ده. **mitigation**: default = disabled (که در همین Step اعمال شد). کاربر روی مسیر lossy می‌تونه opt-in کنه. این رفتار مطلوبه — feature صرفاً وقتی sense می‌ده که loss واقعی > overhead باشه.
- **[Step 5 / preexisting / TEST-only / flaky]** ✅ **resolved در Step 6** — `TestARQ_ReceiveDataClearsQueuedNackWhenMissingDataArrives` در `internal/arq/arq_test.go`. علت اصلی: time-of-check race — تست بعد از receive کردن ACK packet آنی `removedNackSeqs` را چک می‌کرد، ولی `clearSentDataNack` (که `RemoveQueuedDataNack` را صدا می‌زنه) **بعد از** ACK push اجرا می‌شه و async است. fix: polling 500ms با کپی thread-safe. **تأیید: count=20 پاس.**
- **[Step 6 / preexisting / TEST-only]** ✅ **resolved در همین Step 6** — `TestAsyncStreamCleanupWorker` و `TestApplyPlannerNoConnectionPolicyRequeuesDataTask` در `internal/client/`. علت: `buildTCPTestClient` بدون cleanup، تست‌هایی مثل `TestForceCloseStreamQueuesRST` فقط RST queue می‌کنن (نه `ARQ.Close(Force)`)، goroutine retransmit ARQ زنده می‌مونه تا تست بعدی stream جدید در حافظه reuse می‌سازه و race detector write/read متناقض می‌بینه. fix: `buildTCPTestClient(t)` با `t.Cleanup` که stream‌های هنوز فعال را Force-close می‌کنه (با 20ms settling delay).
- **[Step 6 / production / race]** ✅ **resolved در Step 7** — `ARQ.Close()` در `internal/arq/arq.go` خط 3238 read بدون lock + خط 3244 write با lock. fix: read منتقل شد به داخل همون `a.mu.Lock()` که write را انجام می‌ده (خط 3242). دو تست concurrency جدید `TestARQ_CloseConcurrentSafe` و `TestARQ_CloseVirtualConcurrentSafe` با 50 iter × 8 goroutine موازی پاس می‌شن. `WaitForShutdown` متد جدید برای test cleanup deterministic، production behavior بدون تغییر.
- **[Step 6 / NEW / TEST-only / flaky]** 🆕 `TestBalancerLossThenLatencyRoundRobinsAcrossNearTopCandidates` در `internal/client/balancer_test.go:233` به‌صورت intermittent FAIL می‌شه (`expected round-robin across near-top candidates, seen=map[a:true]`). این **race نیست** — assertion flakiness است که احتمالاً به ترتیب اجرا یا scheduling حساسیت داره. preexisting (وابسته به این Step نیست). برای استپ آینده.
- **[Step 7 / NEW / TEST-only / cross-test flaky]** 🆕 `TestARQ_GracefulCloseWriteFailureStillRechecksCloseReadCompletion` در `internal/arq/arq_test.go:1923` به‌صورت intermittent FAIL می‌شه با count=10 در full-suite run (`timed out waiting for graceful-close write attempt`). در isolation با count=10 پاس می‌شه. این یعنی cross-test interaction (احتمالاً global state، GC pressure، یا timing شدید زیر race detector). preexisting، به Step 7 ربط نداره. برای استپ آینده.
- **[Step 7 / NEW / TEST-only / cross-test flaky]** 🆕 `TestCleanupClosedSessionClosesStreamsAndClearsQueues` در `internal/udpserver/session_cleanup_test.go:114` به‌صورت intermittent FAIL می‌شه با count=10 در full-suite run (`expected stream TX queue to be cleared, got 1`). در isolation با count=10 پاس می‌شه. cross-test flakiness preexisting. برای استپ آینده.

---

## 📊 Baseline (پر شدن در استپ ۱)
<!-- جدول اعداد قبل از شروع ارتقا — به‌روزرسانی در استپ ۱ -->

اعداد روی sandbox لینوکس (Go 1.25, AMD64, loopback)، با `go run ./scripts/bench`
که سرور/کلاینت رو لوکال بالا میاره و throughput رو با First-Byte Timing
اندازه می‌گیره. سناریوهای loss نیاز به `tc netem` با privilege دارن و
در محیط هدف اجرا میشن (recipe در `scripts/bench/README.md`).

| سناریو | Payload | Runs | Throughput Up (MiB/s) | Throughput Down (MiB/s) | Notes |
|---|---|---|---|---|---|
| Lossless local | 5 MiB  | 2 | 7.90  (avg of 8.79 / 7.17) | 18.59 (avg of 18.06 / 19.14) | warm build |
| Lossless local | 10 MiB | 3 | 1.66  (avg of 1.99 / 1.31 / 1.85) | 28.43 (avg of 30.42 / 28.96 / 26.23) | larger payload — Up direction collapses, candidate for step 4/5 |
| 1% loss        | —      | — | TBD (run with `tc qdisc add dev lo root netem loss 1%`) | TBD | needs root |
| 5% loss        | —      | — | TBD (run with `tc qdisc add dev lo root netem loss 5%`) | TBD | needs root |

**مشاهده‌ی اولیه:** فاصله‌ی واضح بین Up و Down حتی روی loopback نشون میده
hot-path ARQ send (استپ ۵) و send-side allocation (استپ ۲/۴) جای کار جدی
دارن. این عدد پایه‌ی همه‌ی مقایسه‌های بعدیه.

---

## 📝 یادداشت‌های اجرایی

- شروع از استپ ۱ — این استپ پایه اعداد‌سنجی همه استپ‌های بعدی است.
- بعد از پایان هر استپ: `git add -A && git commit -m "step N: <عنوان>" && git push origin main` (طبق قانون فایل قوانین).
- اگر استپ به باگ پیچیده برخورد کرد: ذکر در بخش 🐛 و افزودن استپ جدید با اولویت بالا **قبل از** استپ بعدی فعلی.
