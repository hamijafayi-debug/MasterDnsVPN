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
- [ ] استپ ۳ — Logging Fast-Path: حذف رشته‌سازی در سطح Debug غیرفعال
- [ ] استپ ۴ — ARQ Receive Path & Buffer Reuse
- [ ] استپ ۵ — ARQ Send Path & Adaptive RTO Tuning
- [ ] استپ ۶ — Balancer Lock Granularity & Selection Fast-Path
- [ ] استپ ۷ — UDP Server Ingress: Batch Read & Worker Sizing
- [ ] استپ ۸ — Session Store Sharding (server-side)
- [ ] استپ ۹ — DNS Parser Zero-Copy & Reusable Decoders
- [ ] استپ ۱۰ — Compression Pools & Threshold Heuristics
- [ ] استپ ۱۱ — Crypto Hot-Path: AEAD nonce reuse & buffer alignment
- [ ] استپ ۱۲ — MTU Discovery: همگرایی سریع‌تر و backoff هوشمند
- [ ] استپ ۱۳ — Resolver Health: تشخیص سریع‌تر outage و reactivation هوشمند
- [ ] استپ ۱۴ — Duplication Policy: انتخاب وفقی به جای ثابت
- [ ] استپ ۱۵ — SOCKS5 Upstream: connection pooling و reuse
- [ ] استپ ۱۶ — Cache Layer: dnscache زیرساخت hot/cold و prune بهینه
- [ ] استپ ۱۷ — Goroutine Audit & Lifecycle (نشت‌یاب)
- [ ] استپ ۱۸ — Backpressure & Bounded Queues تمام لایه‌ها
- [ ] استپ ۱۹ — CI Regression Bench (محافظ سرعت در PR‌ها)
- [ ] استپ ۲۰ — Race & Fuzz Sweep
- [ ] استپ ۲۱ — Release Hardening (build flags, PGO, strip, GOAMD64)

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
- [ ] افزودن متد `DebugEnabled()/InfoEnabled()` به `internal/logger`
- [ ] محصور کردن ۱۰۷ نقطه `Debugf` در hot-path با `if log.DebugEnabled()`
- [ ] استفاده از `strconv` به جای `fmt.Sprintf` در جاهایی که پشت Debug هستن و حتی در no-op هزینه‌بر‌اند
- [ ] افزودن benchmark `BenchmarkLoggerDisabled` برای تضمین صفر تخصیص
- [ ] ثبت آمار GC قبل/بعد در `PLAN.md` (با ۳ دقیقه بنچ تحت بار)

### استپ ۴ — ARQ Receive Path & Buffer Reuse
هدف: کاهش allocation و کپی در RX.
- [ ] بازنویسی `rxPayload` و `arqDataItem.Data` با buffer reuse از pool
- [ ] حذف کپی غیرضروری در `contiguousReadyLocked` و write به localConn (writev اگر ممکن)
- [ ] افزودن `BenchmarkARQReceiveInOrder` و `BenchmarkARQReceiveOutOfOrder`
- [ ] تست واحد جدید برای edge case: out-of-order + duplicate تحت loss 10%
- [ ] رصد retx counter اضافه‌شده در استپ ۱

### استپ ۵ — ARQ Send Path & Adaptive RTO Tuning
هدف: کاهش retx غیرضروری و افزایش throughput.
- [ ] بازبینی `updateAdaptiveRTO`: clamp های فعلی، α/β استاندارد RFC 6298، minRTO کف‌بندی هوشمند بر اساس RTT اخیر
- [ ] افزودن early-retransmit (مشابه fast-retransmit در TCP) با شمارش dup-ACK
- [ ] محدودسازی تعداد retx در پنجره زمانی برای جلوگیری از retx storm
- [ ] افزودن knob `ARQ_FAST_RETX_THRESHOLD` در کانفیگ کلاینت و سرور
- [ ] بنچ تحت 2% و 5% loss و گزارش throughput

### استپ ۶ — Balancer Lock Granularity & Selection Fast-Path
هدف: کاهش contention روی balancer mutex وقتی resolver زیاد است.
- [ ] تفکیک قفل خواندن `GetBestConnection` از قفل نوشتن stats — `RWMutex` و read-mostly path
- [ ] cache رتبه‌بندی resolverها در snapshot بدون قفل، بازسازی فقط هنگام تغییر
- [ ] افزودن `BenchmarkBalancerSelect` با ۵۰ resolver
- [ ] تست واحد invariant: snapshot view و state واقعی نباید واگرا شوند
- [ ] گزارش µs/op قبل و بعد

### استپ ۷ — UDP Server Ingress: Batch Read & Worker Sizing
هدف: throughput بالاتر روی سرور با کارگران بهینه و batching.
- [ ] افزودن مسیر batch با `golang.org/x/net/ipv4.PacketConn.ReadBatch` در لینوکس (build tag)
- [ ] auto-tuning `UDP_READERS` بر اساس `GOMAXPROCS` با کف و سقف منطقی
- [ ] فال‌بک خودکار به single ReadFrom در پلتفرم‌های بدون پشتیبانی
- [ ] بنچ لوکال 1M packet/sec و گزارش drops
- [ ] تست واحد ingress برای فال‌بک

### استپ ۸ — Session Store Sharding (server-side)
هدف: حذف bottleneck قفل سراسری sessions در سرور پرترافیک.
- [ ] sharding `sessionStore` به N=64 شارد بر اساس hash(SessionID)
- [ ] حفظ API فعلی (هیچ کد فراخواننده‌ای نشکند)
- [ ] افزودن بنچ‌مارک concurrent insert/lookup
- [ ] تست واحد: cleanup correctness روی شاردها
- [ ] گزارش kops/sec قبل و بعد

### استپ ۹ — DNS Parser Zero-Copy
هدف: حذف allocation در پارس کوئری/پاسخ.
- [ ] افزودن decoder قابل reuse با state داخلی pool
- [ ] استفاده از `slices` view به جای کپی برای label/name parsing
- [ ] افزودن fuzz target در `internal/dnsparser` (Go fuzz)
- [ ] بنچ‌مارک قبل/بعد در `BenchmarkParseQuery`/`BenchmarkBuildResponse`
- [ ] گزارش allocs/op و B/op

### استپ ۱۰ — Compression Pools & Threshold Heuristics
هدف: کاهش هزینه compression بدون از دست دادن نسبت.
- [ ] reuse encoder/decoder های zstd و lz4 با pool (در حال حاضر هر فراخوانی encoder جدید؟ بررسی شود)
- [ ] افزودن آستانه `MIN_COMPRESS_BYTES` — payload کمتر از این، رد می‌شود
- [ ] انتخاب وفقی الگوریتم بر اساس entropy تخمینی payload
- [ ] بنچ روی payload های مصنوعی random/text/binary
- [ ] ثبت knob ها در کانفیگ نمونه

### استپ ۱۱ — Crypto Hot-Path
هدف: کاهش هزینه AEAD و حذف allocation.
- [ ] reuse `cipher.AEAD` با pool به ازای هر nonce-builder
- [ ] buffer alignment برای ChaCha20 (افزایش throughput روی ARM)
- [ ] افزودن `BenchmarkCodecSealOpen` با اندازه‌های واقعی payload
- [ ] تست fuzz روی codec
- [ ] گزارش MB/s قبل و بعد

### استپ ۱۲ — MTU Discovery
هدف: همگرایی سریع‌تر MTU با ثبات بیشتر روی resolverهای سخت‌گیر.
- [ ] بازبینی binary-search probe — gap-pruning و early-exit وقتی fail consistent
- [ ] backoff نمایی برای probe ناموفق + jitter
- [ ] افزودن knob `MTU_PROBE_AGGRESSIVE` (پیش‌فرض false)
- [ ] تست واحد سناریوی MTU outlier (که از commit اخیر هم اضافه شده)
- [ ] گزارش زمان همگرایی روی ۵ resolver متفاوت

### استپ ۱۳ — Resolver Health: تشخیص سریع‌تر outage
هدف: کم کردن زمان stuck روی resolver بد.
- [ ] کاهش پنجره auto-disable به طور وفقی وقتی active count بالا است (در `balancer.go` منطق فعلی هست — بهینه شود)
- [ ] reactivation با شیب تدریجی (gradual ramp-up) به جای فعال‌شدن یکدفعه
- [ ] افزودن circuit-breaker سبک
- [ ] تست سناریوی blackhole resolver
- [ ] گزارش p95 stuck-time قبل و بعد

### استپ ۱۴ — Duplication Policy: وفقی
هدف: ارسال duplicate فقط در مواقع لازم به جای ثابت.
- [ ] افزودن متریک loss تخمینی per-resolver
- [ ] فعال‌سازی duplication فقط وقتی loss > آستانه قابل تنظیم
- [ ] knob جدید `ADAPTIVE_DUPLICATION` (پیش‌فرض خاموش برای backward compat)
- [ ] تست واحد policy switching
- [ ] مقایسه bandwidth-overhead قبل و بعد روی scenario lossy

### استپ ۱۵ — SOCKS5 Upstream Connection Pooling
هدف: کاهش latency در حالت `UseExternalSOCKS5`.
- [ ] افزودن idle-pool برای کانکشن‌های upstream SOCKS5 با TTL
- [ ] reuse handshake نتیجه برای same destination در پنجره کوتاه
- [ ] knob: `SOCKS5_POOL_IDLE`, `SOCKS5_POOL_MAX`
- [ ] تست واحد pool eviction و TTL
- [ ] گزارش mean connect-time

### استپ ۱۶ — DNS Cache Layer
هدف: کاهش lookup سرور وقتی سرور resolve محلی هم انجام می‌دهد.
- [ ] تقسیم cache به hot tier (in-memory LRU کوچک و سریع) و cold tier (فعلی)
- [ ] prune دوره‌ای با amortized cost پایین (به جای scan کامل)
- [ ] بنچ hit-rate و lookup latency
- [ ] تست TTL accuracy
- [ ] رصد cache_hits / cache_misses در expvar (از استپ ۱)

### استپ ۱۷ — Goroutine Audit & Lifecycle
هدف: حذف نشت goroutine و تضمین خاتمه روی shutdown.
- [ ] فهرست همه `go func` ها (۳۰+ مورد) با محل و مسیر خاتمه
- [ ] افزودن تست `TestNoGoroutineLeak` با `goleak`-style assertion
- [ ] رفع نشت‌های یافت‌شده (هرکدام = یک عنوان زیر `## 🐛 باگ‌های یافته‌شده` اگر باگ بود)
- [ ] افزودن hard-stop budget برای shutdown سرور و کلاینت
- [ ] گزارش تعداد goroutine قبل/بعد در حالت idle طولانی

### استپ ۱۸ — Backpressure & Bounded Queues
هدف: جلوگیری از انفجار حافظه تحت بار سنگین.
- [ ] ممیزی همه channel‌های `make(chan ..., N)` و توجیه N
- [ ] افزودن drop-with-counter (به‌جای block بی‌نهایت) در ingress
- [ ] knob: `INGRESS_DROP_POLICY` (drop-newest / drop-oldest)
- [ ] تست شبیه‌سازی burst و سنجش memory ceiling
- [ ] گزارش peak RSS قبل و بعد

### استپ ۱۹ — CI Regression Bench
هدف: PR بد سرعت را زمین نزند.
- [ ] افزودن workflow جدید `bench.yml` که `go test -bench` روی پکیج‌های کلیدی اجرا و output را در PR کامنت کند
- [ ] threshold check ساده (regression > 10% → fail)
- [ ] حافظه نتایج تاریخی در branch جدا (artifacts) — سبک
- [ ] مستندسازی در README
- [ ] فعال‌سازی برای push روی main و PR

### استپ ۲۰ — Race & Fuzz Sweep
هدف: شکار باگ‌های پنهان قبل از prod.
- [ ] اجرای `go test -race ./...` و رفع warnings (هرکدام جدا گزارش)
- [ ] افزودن fuzz target برای `vpnproto/parser`, `dnsparser/parser`, `security/codec`
- [ ] فعال‌سازی fuzz در CI با budget کوتاه (۳۰ ثانیه per target)
- [ ] رفع crashing input ها
- [ ] گزارش پوشش fuzz در README

### استپ ۲۱ — Release Hardening
هدف: بیشترین سرعت در باینری نهایی.
- [ ] فعال‌سازی PGO با profile جمع‌آوری‌شده از bench طولانی
- [ ] افزودن `-trimpath -ldflags="-s -w"` به همه matrix builds
- [ ] فعال‌سازی `GOAMD64=v3` برای builds مدرن (با fallback)
- [ ] تست smoke هر باینری روی هر OS/ARCH
- [ ] گزارش حجم باینری و سرعت بنچ نهایی قبل/بعد

---

## 🐛 باگ‌های یافته‌شده
<!-- هنگام برخورد باگ در حین استپ، اینجا یک‌خطی ثبت می‌شود -->

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
