// benchregress — مقایسه دو خروجی `go test -bench` و تشخیص رگرسیون.
//
// این ابزار خودکفا و بدون وابستگی خارجی است (فقط stdlib).
// Workflow:
//   1) baseline.txt را از branch هدف (یا artifact ذخیره‌شده) بارگذاری می‌کند.
//   2) current.txt را از این run بارگذاری می‌کند.
//   3) برای هر بنچ موجود در هر دو فایل، Δ ns/op و Δ B/op را محاسبه می‌کند.
//   4) اگر هر بنچی > threshold (پیش‌فرض 10%) کند شده، با exit code 1 خارج می‌شود.
//   5) یک جدول markdown روی stdout و یک خلاصه روی stderr چاپ می‌کند.
//
// فرمت ورودی: خروجی استاندارد `go test -bench=. -benchmem`.
// خطوط مدنظر:
//   BenchmarkName-N    iters    ns/op    B/op allocs/op
//
// استفاده:
//   benchregress -baseline baseline.txt -current current.txt \
//                -threshold 10 -markdown out.md
//
// نکات:
//   - بنچ‌های فقط در یکی از فایل‌ها (added/removed) با گزارش می‌شوند ولی fail نمی‌کنند.
//   - regression روی ns/op اندازه‌گیری می‌شود (سرعت)؛ B/op و allocs/op صرفاً گزارش می‌شوند.
//   - نتایج با count>1 → mean ساده محاسبه می‌شود (median/stddev در v2).
package main

import (
	"bufio"
	"flag"
	"fmt"
	"os"
	"sort"
	"strconv"
	"strings"
)

// BenchResult یک نتیجه از یک خط بنچ‌مارک Go.
type BenchResult struct {
	Name      string
	NsPerOp   float64 // مقدار میانگین در صورت چندتایی
	BytesPerOp float64
	AllocsPerOp float64
	Samples   int // تعداد نمونه‌های جمع‌آوری‌شده
}

func main() {
	baselinePath := flag.String("baseline", "baseline.txt", "path to baseline bench output")
	currentPath := flag.String("current", "current.txt", "path to current bench output")
	threshold := flag.Float64("threshold", 10.0, "regression threshold percent (ns/op) — fail if any bench is slower by more than this")
	markdownPath := flag.String("markdown", "", "if set, write a markdown report to this path")
	failOnRegression := flag.Bool("fail-on-regression", true, "exit code 1 on any ns/op regression > threshold")
	flag.Parse()

	baseline, err := parseFile(*baselinePath)
	if err != nil {
		fmt.Fprintf(os.Stderr, "ERROR: cannot read baseline %q: %v\n", *baselinePath, err)
		os.Exit(2)
	}
	current, err := parseFile(*currentPath)
	if err != nil {
		fmt.Fprintf(os.Stderr, "ERROR: cannot read current %q: %v\n", *currentPath, err)
		os.Exit(2)
	}

	fmt.Fprintf(os.Stderr, "[benchregress] baseline benches: %d\n", len(baseline))
	fmt.Fprintf(os.Stderr, "[benchregress] current  benches: %d\n", len(current))

	// نام‌های مشترک
	names := mergeNames(baseline, current)

	type Row struct {
		Name      string
		BaseNs    float64
		CurNs     float64
		DiffPct   float64
		Regressed bool
		Improved  bool
		Status    string // "ok", "regressed", "improved", "added", "removed"
		BaseB     float64
		CurB      float64
		BaseAllocs float64
		CurAllocs float64
	}
	rows := make([]Row, 0, len(names))
	worstReg := 0.0
	worstRegName := ""
	regCount := 0
	impCount := 0

	for _, name := range names {
		b, hasB := baseline[name]
		c, hasC := current[name]
		switch {
		case hasB && hasC:
			diff := pctDiff(b.NsPerOp, c.NsPerOp)
			r := Row{
				Name:    name,
				BaseNs:  b.NsPerOp,
				CurNs:   c.NsPerOp,
				DiffPct: diff,
				BaseB:   b.BytesPerOp,
				CurB:    c.BytesPerOp,
				BaseAllocs: b.AllocsPerOp,
				CurAllocs:  c.AllocsPerOp,
			}
			switch {
			case diff > *threshold:
				r.Status = "regressed"
				r.Regressed = true
				regCount++
				if diff > worstReg {
					worstReg = diff
					worstRegName = name
				}
			case diff < -(*threshold):
				r.Status = "improved"
				r.Improved = true
				impCount++
			default:
				r.Status = "ok"
			}
			rows = append(rows, r)
		case hasB && !hasC:
			rows = append(rows, Row{Name: name, BaseNs: b.NsPerOp, Status: "removed"})
		case !hasB && hasC:
			rows = append(rows, Row{Name: name, CurNs: c.NsPerOp, Status: "added"})
		}
	}

	// مرتب‌سازی: بدترین regression‌ها در بالا، بعد improved، بعد ok
	sort.SliceStable(rows, func(i, j int) bool {
		// status priority
		prio := func(s string) int {
			switch s {
			case "regressed":
				return 0
			case "added":
				return 1
			case "removed":
				return 2
			case "improved":
				return 3
			default:
				return 4
			}
		}
		pi, pj := prio(rows[i].Status), prio(rows[j].Status)
		if pi != pj {
			return pi < pj
		}
		// در همان priority، diff بزرگ‌تر اول
		return rows[i].DiffPct > rows[j].DiffPct
	})

	compared := 0
	for _, r := range rows {
		if r.Status == "regressed" || r.Status == "improved" || r.Status == "ok" {
			compared++
		}
	}

	// خروجی stderr — خلاصه
	fmt.Fprintf(os.Stderr, "[benchregress] threshold: %.1f%%\n", *threshold)
	fmt.Fprintf(os.Stderr, "[benchregress] regressed: %d   improved: %d   total compared: %d\n",
		regCount, impCount, compared)
	if regCount > 0 {
		fmt.Fprintf(os.Stderr, "[benchregress] worst regression: %s (+%.2f%%)\n", worstRegName, worstReg)
	}

	// خروجی stdout — markdown
	var sb strings.Builder
	sb.WriteString("## Benchmark Regression Report\n\n")
	sb.WriteString(fmt.Sprintf("- **Threshold:** %.1f%% (ns/op)\n", *threshold))
	sb.WriteString(fmt.Sprintf("- **Regressed:** %d benches above threshold\n", regCount))
	sb.WriteString(fmt.Sprintf("- **Improved:** %d benches below -threshold\n", impCount))
	if regCount > 0 {
		sb.WriteString(fmt.Sprintf("- **Worst regression:** `%s` (+%.2f%%)\n", worstRegName, worstReg))
	}
	sb.WriteString("\n")

	// جدول
	sb.WriteString("| Status | Benchmark | Baseline ns/op | Current ns/op | Δ% | Baseline B/op | Current B/op |\n")
	sb.WriteString("|---|---|---:|---:|---:|---:|---:|\n")
	for _, r := range rows {
		var icon string
		switch r.Status {
		case "regressed":
			icon = "🔴 REG"
		case "improved":
			icon = "🟢 IMP"
		case "added":
			icon = "➕ NEW"
		case "removed":
			icon = "➖ DEL"
		default:
			icon = "✅ ok"
		}
		baseNs := formatFloat(r.BaseNs)
		curNs := formatFloat(r.CurNs)
		diffStr := ""
		if r.Status == "regressed" || r.Status == "improved" || r.Status == "ok" {
			diffStr = fmt.Sprintf("%+.2f%%", r.DiffPct)
		}
		baseB := formatFloat(r.BaseB)
		curB := formatFloat(r.CurB)
		sb.WriteString(fmt.Sprintf("| %s | `%s` | %s | %s | %s | %s | %s |\n",
			icon, escapeMD(r.Name), baseNs, curNs, diffStr, baseB, curB))
	}
	sb.WriteString("\n")
	if regCount == 0 {
		sb.WriteString("**✅ No regressions detected.**\n")
	} else {
		sb.WriteString(fmt.Sprintf("**🔴 %d regression(s) above %.1f%% threshold.**\n", regCount, *threshold))
	}

	fmt.Print(sb.String())

	if *markdownPath != "" {
		if err := os.WriteFile(*markdownPath, []byte(sb.String()), 0o644); err != nil {
			fmt.Fprintf(os.Stderr, "ERROR: cannot write markdown %q: %v\n", *markdownPath, err)
			os.Exit(2)
		}
		fmt.Fprintf(os.Stderr, "[benchregress] markdown written → %s\n", *markdownPath)
	}

	if *failOnRegression && regCount > 0 {
		os.Exit(1)
	}
}

// parseFile یک فایل خروجی `go test -bench` را خوانده و map نام→نتیجه برمی‌گرداند.
// در صورت چند sample برای یک نام (count>1)، mean ساده محاسبه می‌شود.
func parseFile(path string) (map[string]*BenchResult, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()

	out := make(map[string]*BenchResult)
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 1<<20), 1<<20)
	for sc.Scan() {
		line := sc.Text()
		r, ok := parseLine(line)
		if !ok {
			continue
		}
		if prev, exists := out[r.Name]; exists {
			// running mean
			n := float64(prev.Samples)
			prev.NsPerOp = (prev.NsPerOp*n + r.NsPerOp) / (n + 1)
			prev.BytesPerOp = (prev.BytesPerOp*n + r.BytesPerOp) / (n + 1)
			prev.AllocsPerOp = (prev.AllocsPerOp*n + r.AllocsPerOp) / (n + 1)
			prev.Samples++
		} else {
			r.Samples = 1
			cp := r
			out[r.Name] = &cp
		}
	}
	if err := sc.Err(); err != nil {
		return nil, err
	}
	return out, nil
}

// parseLine یک خط بنچ Go را پارس می‌کند.
// نمونه:
//   BenchmarkARQ_PushRecv-8    1234567    123.4 ns/op    16 B/op    1 allocs/op
//
// نام بنچ همراه با suffix -N (تعداد CPU) باقی می‌ماند تا تطبیق دقیق باشد.
func parseLine(line string) (BenchResult, bool) {
	line = strings.TrimSpace(line)
	if !strings.HasPrefix(line, "Benchmark") {
		return BenchResult{}, false
	}
	fields := strings.Fields(line)
	if len(fields) < 3 {
		return BenchResult{}, false
	}
	// fields[0] = Name, fields[1] = iter count, بقیه واحد-جفت
	r := BenchResult{Name: fields[0]}
	// از field 2 به بعد، جفت‌های "value unit"
	for i := 2; i+1 < len(fields); i += 2 {
		val, err := strconv.ParseFloat(fields[i], 64)
		if err != nil {
			continue
		}
		unit := fields[i+1]
		switch unit {
		case "ns/op":
			r.NsPerOp = val
		case "B/op":
			r.BytesPerOp = val
		case "allocs/op":
			r.AllocsPerOp = val
		}
	}
	if r.NsPerOp == 0 {
		return BenchResult{}, false
	}
	return r, true
}

func mergeNames(a, b map[string]*BenchResult) []string {
	set := make(map[string]struct{}, len(a)+len(b))
	for k := range a {
		set[k] = struct{}{}
	}
	for k := range b {
		set[k] = struct{}{}
	}
	out := make([]string, 0, len(set))
	for k := range set {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

func pctDiff(base, cur float64) float64 {
	if base == 0 {
		return 0
	}
	return (cur - base) / base * 100.0
}

func formatFloat(v float64) string {
	if v == 0 {
		return "—"
	}
	if v >= 1000 {
		return fmt.Sprintf("%.0f", v)
	}
	return fmt.Sprintf("%.2f", v)
}

func escapeMD(s string) string {
	// pipe character must be escaped in markdown tables
	return strings.ReplaceAll(s, "|", "\\|")
}
