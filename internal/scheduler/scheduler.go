// Package scheduler — P1.5 — wires detector → selector → worktree → probes →
// differ → bundle into one verify run.
//
// Ordering guarantees that matter for evidence validity:
//   - the Selection is computed once and the SAME target set is applied to both
//     worktrees (base-full vs candidate-narrowed would fabricate deltas);
//   - only probes that have a command in the profile run (missing ecosystems are
//     skipped, not turned into inconclusive noise);
//   - base-side results are cached by (base_ref, probe, env, targets) so agent
//     repair loops skip re-running an unchanged base;
//   - budget exhaustion is recorded in unverified_claims, never silently dropped.
package scheduler

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/junqingyongyuanbusi/vouch/internal/bundle"
	"github.com/junqingyongyuanbusi/vouch/internal/cache"
	"github.com/junqingyongyuanbusi/vouch/internal/detector"
	"github.com/junqingyongyuanbusi/vouch/internal/differ"
	"github.com/junqingyongyuanbusi/vouch/internal/gitexclude"
	"github.com/junqingyongyuanbusi/vouch/internal/probe"
	"github.com/junqingyongyuanbusi/vouch/internal/probe/builtin"
	"github.com/junqingyongyuanbusi/vouch/internal/sandbox"
	"github.com/junqingyongyuanbusi/vouch/internal/selector"
	"github.com/junqingyongyuanbusi/vouch/internal/verdict"
	"github.com/junqingyongyuanbusi/vouch/internal/worktree"
)

// Config controls one verify run.
type Config struct {
	RepoRoot      string
	BaseRef       string // default "HEAD"
	CandidateRef  string // optional explicit candidate ref (branch-range mode)
	DiffFiles     []string
	Intent        string
	BudgetMs      int // total wall-clock budget (default 600_000)
	ProbeBudgetMs map[string]int
	DisableCache  bool
	// InstallDeps opts in to a best-effort dependency bootstrap (JS only).
	// Default OFF: a verifier must not mutate the audited repository or hit the
	// network unless the user asked for it. Without it, a missing runner is
	// reported as an actionable gap instead.
	InstallDeps bool
	// DepsBudgetMs bounds the bootstrap step when InstallDeps is set (default 300_000).
	DepsBudgetMs int
}

// Result is everything the CLI/render layer needs.
type Result struct {
	Profile         bundle.ProjectProfile
	Selection       selector.Selection
	Evidences       []bundle.Evidence
	Warnings        []string
	Unverified      []string
	Verdict         bundle.GlobalVerdict
	Bundle          bundle.ProofBundle
	SnapshotRef     string // refs/vouch/snapshots/<sha> ("" when the tree was clean)
	CandidateCommit string // commit actually verified (snapshot or HEAD)
}

// Run executes the full differential verify pipeline.
func Run(ctx context.Context, cfg Config) (res Result, err error) {
	if cfg.RepoRoot == "" {
		cfg.RepoRoot = "."
	}
	absRepo, err := filepath.Abs(cfg.RepoRoot)
	if err != nil {
		return Result{}, err
	}
	cfg.RepoRoot = absRepo
	if cfg.BaseRef == "" {
		cfg.BaseRef = "HEAD"
	}
	// The budget is the TOTAL wall-clock deadline for the whole run, and the
	// scheduler owns it: the deadline starts here — before detection, worktree
	// setup and any probe — so the three probes plus setup share one budget
	// instead of each command getting its own. Context-aware commit resolution,
	// dependency bootstrap, worktree setup (git snapshot/add, cp) and probes
	// observe this deadline: their process groups are killed and reaped on
	// expiry. Pure filesystem cleanup (os.RemoveAll, hardlink trees) is not
	// deadline-bounded, so this is still not a strict bound on Run's return.
	// The value is clamped before the Duration conversion: a huge BudgetMs would
	// overflow int64 nanoseconds into a negative Duration, and
	// context.WithTimeout would then expire instantly (spurious UNVERIFIED).
	if cfg.BudgetMs <= 0 {
		cfg.BudgetMs = DefaultBudgetMs
	}
	if cfg.BudgetMs > MaxBudgetMs {
		cfg.BudgetMs = MaxBudgetMs
	}
	ctx, cancel := context.WithTimeout(ctx, time.Duration(cfg.BudgetMs)*time.Millisecond)
	defer cancel()

	// Resolve to a concrete commit: a symbolic ref (HEAD/main) moves between
	// runs, and using it as a cache key would reuse an outdated baseline while
	// the candidate side is a new snapshot (silent false verdicts).
	baseSHA, err := resolveCommit(ctx, cfg.RepoRoot, cfg.BaseRef)
	if err != nil {
		if res, ok := setupBudgetResult(ctx, cfg, fmt.Sprintf("resolve base %q", cfg.BaseRef), err); ok {
			return res, nil
		}
		return Result{}, fmt.Errorf("resolve base %q: %w", cfg.BaseRef, err)
	}
	cfg.BaseRef = baseSHA
	if cfg.CandidateRef != "" {
		candSHA, err := resolveCommit(ctx, cfg.RepoRoot, cfg.CandidateRef)
		if err != nil {
			if res, ok := setupBudgetResult(ctx, cfg, fmt.Sprintf("resolve candidate %q", cfg.CandidateRef), err); ok {
				return res, nil
			}
			return Result{}, fmt.Errorf("resolve candidate %q: %w", cfg.CandidateRef, err)
		}
		cfg.CandidateRef = candSHA
	}

	profile, err := detector.Detect(cfg.RepoRoot)
	if err != nil {
		return Result{}, fmt.Errorf("detector: %w", err)
	}

	// Make vouch's own state invisible to the target repo before snapshotting.
	// A failure here would silently leak node_modules/.vouch into the snapshot,
	// so it is reported instead of dropped.
	if err := gitexclude.Add(cfg.RepoRoot, ".vouch/", "node_modules/"); err != nil {
		profile.Gaps = append(profile.Gaps, "local git exclude unavailable: "+err.Error()+" (vouch/node_modules may appear in the diff)")
	}
	profile.Gaps = append(profile.Gaps, bootstrapDeps(ctx, cfg, profile)...)
	// (dependency-reuse gaps are appended after the worktree pair is created)

	pair, err := worktree.CreateContext(ctx, cfg.RepoRoot, cfg.BaseRef, cfg.CandidateRef)
	if err != nil {
		// A canceled or expired budget during worktree setup (snapshot,
		// worktree add, dependency copy) is an honest UNVERIFIED with
		// attribution, not a tool error the agent cannot act on.
		if res, ok := setupBudgetResult(ctx, cfg, "worktree setup", err); ok {
			return res, nil
		}
		return Result{}, fmt.Errorf("worktree: %w", err)
	}
	// Surface cleanup failures instead of swallowing them: a leftover
	// worktree registration or directory is user-visible state.
	defer func() {
		if cerr := pair.Cleanup(); cerr != nil {
			res.Warnings = append(res.Warnings, "worktree cleanup failed: "+cerr.Error())
		}
	}()

	// Diff source is base → candidate snapshot: the snapshot contains untracked
	// files, so a "pure addition" is visible (git diff HEAD alone would report
	// no change and collide with the empty-diff case).
	profile.Gaps = append(profile.Gaps, pair.DepsGaps...)
	diffBytes, diffFiles := DiffBetween(cfg.RepoRoot, cfg.BaseRef, pair.CandidateCommit)
	if len(cfg.DiffFiles) > 0 {
		diffFiles = cfg.DiffFiles
	}
	diffFiles = filterSelfArtifacts(diffFiles)
	sel := selector.Select(pair.Candidate, diffFiles, profile)
	profile = selector.NewProfileWithSelection(profile, sel)

	bundleID := bundle.BundleIDFor(diffBytes, cfg.BaseRef)

	// Decide once whether the narrow selection can be shared by both sides. The
	// decision is made BEFORE the cache key is derived, so a cached entry always
	// describes the command that will actually run (key/result must not diverge).
	narrowTest := false
	testCommand := ""
	testBaseCommand := ""
	if cmd, ok := profile.Commands["test"]; ok && strings.TrimSpace(cmd.Cmd) != "" {
		baseSel := selector.RestrictToExisting(pair.Base, sel, profile)
		candSel := selector.RestrictToExisting(pair.Candidate, sel, profile)
		if baseSel.FullRun || candSel.FullRun ||
			len(baseSel.Targets) != len(sel.Targets) || len(candSel.Targets) != len(sel.Targets) {
			full := selector.Selection{FullRun: true, Reason: "sides cannot share the candidate target set"}
			testCommand = selector.Apply(profile, full)
			testBaseCommand = testCommand
			if !sel.FullRun {
				// A narrowing was downgraded: say so, and record why.
				profile.TestSelection = &bundle.TestSelection{Supported: false, Mechanism: "full run (sides cannot share targets)"}
				profile.Gaps = append(profile.Gaps, "test selection downgraded to full run: sides cannot share the candidate target set")
			}
			// When the selector itself chose FullRun (no mappable tests, empty
			// diff, unknown runner) its own mechanism/reason stays truthful.
		} else {
			candCmd, candOK := selector.ApplyNarrowed(profile, sel)
			baseCmd, baseOK := selector.ApplyNarrowed(profile, baseSel)
			if !candOK || !baseOK {
				// The recorded command is not a runner invocation (e.g. `make
				// test`), so appending paths cannot narrow it. Run the project
				// command in full on both sides and say why.
				testCommand = profile.Commands["test"].Cmd
				testBaseCommand = testCommand
				profile.TestSelection = &bundle.TestSelection{Supported: false, Mechanism: "full run (command is not runner-specific)"}
				profile.Gaps = append(profile.Gaps, "test selection disabled: recorded test command is not a runner invocation (e.g. make target)")
			} else {
				narrowTest = true
				testCommand = candCmd
				testBaseCommand = baseCmd
			}
		}
	}
	env := EnvFingerprint(cfg.RepoRoot, profile)
	hashNarrow := cache.TargetsHashFrom(sel.Targets, sel.FullRun)
	hashFull := cache.TargetsHashFrom(nil, true)
	store := cache.NewStore(cfg.RepoRoot)

	// Probes run in information-priority order (typecheck > test > build) so an
	// exhausted budget trims the least valuable probe first. Probes are sequential
	// because they share one worktree (vitest/tsc/build write caches and dist/
	// there). The two sides of a probe run concurrently only when dependency
	// reuse did NOT hardlink node_modules: cp -al shares inodes, so concurrent
	// runners could write the same .vite/.cache files and fake a delta.
	var evidences []bundle.Evidence
	var unverif []string
	var warnings []string
	priority := []string{"typecheck", "test", "build"}

	for _, probeName := range priority {
		cmd, ok := profile.Commands[probeName]
		if !ok || strings.TrimSpace(cmd.Cmd) == "" {
			continue // ecosystem does not provide this probe: skip, don't guess
		}
		if ctx.Err() != nil {
			unverif = append(unverif, probeName+": skipped (budget exhausted)")
			continue
		}
		baseCommand := cmd.Cmd
		command := cmd.Cmd
		targetsHash := hashFull
		if probeName == "test" {
			baseCommand, command = testBaseCommand, testCommand
			if narrowTest {
				targetsHash = hashNarrow
			}
		}
		budget := probeBudget(ctx, cfg, probeName)
		if budget <= 0 {
			// The total budget is spent: the probe is trimmed, not failed. An
			// unmeasured probe must surface as UNVERIFIED, never as a
			// regression or a silent pass.
			unverif = append(unverif, probeName+": skipped (budget exhausted)")
			continue
		}

		baseSide := func() probe.RunResult {
			if cached, hit := loadCached(store, cfg, probeName, env, targetsHash); hit {
				return cached
			}
			res := runBuiltin(ctx, probeName, pair.Base, "base", baseCommand, budget)
			if !cfg.DisableCache && res.Verdict != probe.VerdictInconclusive {
				_ = store.Put(cache.Key{
					BaseRef: cfg.BaseRef, Probe: probeName, EnvFingerprint: env, TargetsHash: targetsHash,
				}, cachedPayload(res))
			}
			return res
		}
		candSide := func() probe.RunResult {
			return runBuiltin(ctx, probeName, pair.Candidate, "candidate", command, budget)
		}
		var baseRes, candRes probe.RunResult
		if pair.DepsReused {
			// Hardlinked node_modules: run the sides one after another so the two
			// runners never write the same cache files concurrently.
			baseRes = baseSide()
			if budgetExhausted(baseRes) {
				// Base could not finish: a candidate comparison would be
				// meaningless, so spend the budget once, not twice.
				candRes = probe.Inconclusive("base side exhausted the probe budget; candidate not run", "candidate")
				unverif = append(unverif, probeName+": base exceeded the probe budget; candidate skipped")
				evidences = append(evidences, mustEvidence(differ.EvidenceWithArbitration(
					probeName,
					fmt.Sprintf("vouch rerun %s --probe %s", bundleID, probeName),
					env, time.Now().UTC(), baseRes, candRes, differ.TestArbitration{})))
				continue
			}
			candRes = candSide()
		} else {
			baseCh := make(chan probe.RunResult, 1)
			candCh := make(chan probe.RunResult, 1)
			go func() { baseCh <- baseSide() }()
			go func() { candCh <- candSide() }()
			baseRes = <-baseCh
			candRes = <-candCh
		}

		if probeName == "test" && narrowTest && emptyNarrowedRun(baseRes, candRes) {
			// The narrowing selected nothing (e.g. an edited file no test
			// imports). Re-run the whole suite on both sides so the verdict is
			// backed by a real signal instead of "no tests found".
			full := selector.Selection{FullRun: true, Reason: "narrowed run selected no tests"}
			fullCmd := selector.Apply(profile, full)
			origMechanism := ""
			if profile.TestSelection != nil {
				origMechanism = profile.TestSelection.Mechanism
			}
			profile.Gaps = append(profile.Gaps, "test selection produced no cases (was: "+origMechanism+"); re-ran the full suite")
			profile.TestSelection = &bundle.TestSelection{Supported: false, Mechanism: "full run (selection matched no tests)"}
			budgetFull := probeBudget(ctx, cfg, probeName)
			// Use the full-run cache key for this fallback: without it every run
			// would re-measure an unchanged base (and the narrowed 0-case result
			// would stay in the cache forever).
			// Do NOT use := here: baseRes must stay the outer variable, or the
			// fallback's base run is discarded and the delta would compare a
			// narrowed base against a full candidate (fabricated regressions).
			var hit bool
			baseRes, hit = loadCached(store, cfg, probeName, env, hashFull)
			if !hit {
				baseRes = runBuiltin(ctx, probeName, pair.Base, "base", fullCmd, budgetFull)
				if !cfg.DisableCache && baseRes.Verdict != probe.VerdictInconclusive {
					_ = store.Put(cache.Key{
						BaseRef: cfg.BaseRef, Probe: probeName, EnvFingerprint: env, TargetsHash: hashFull,
					}, cachedPayload(baseRes))
				}
			}
			candRes = runBuiltin(ctx, probeName, pair.Candidate, "candidate", fullCmd, budgetFull)
			baseCommand, command = fullCmd, fullCmd
			narrowTest = false
		}

		arb := differ.TestArbitration{}
		if probeName == "test" {
			// Only disagreeing cases are re-run; without a case-level filter the
			// arbitration stays unresolved (fail-safe: keep the regression).
			rerun := caseRerunFactory(pair.Base, pair.Candidate, command, budget)
			arb, _ = differ.Arbitrate(ctx, probe.TestFactsOf(baseRes), probe.TestFactsOf(candRes), rerun)
		}
		ev, err := differ.EvidenceWithArbitration(probeName,
			fmt.Sprintf("vouch rerun %s --probe %s", bundleID, probeName),
			env, time.Now().UTC(), baseRes, candRes, arb)
		if err != nil {
			unverif = append(unverif, probeName+": "+err.Error())
			continue
		}
		if ev.Verdict == bundle.EvidenceInconclusive {
			// Name the cause when a side reported one ("command killed by
			// budget", "runner not available"): a bare "inconclusive" leaves
			// the agent unable to tell a tool cap from a repository problem.
			claim := probeName + ": inconclusive"
			if reason := inconclusiveReasonOf(baseRes, candRes); reason != "" {
				claim += " (" + reason + ")"
			}
			unverif = append(unverif, claim)
		}
		// Evidence must name what actually ran. The two-sided form is kept as a
		// guard: today the base and candidate commands are always identical
		// (the fallback sets both, and RestrictToExisting preserves order), but
		// if that ever changes the bundle must say so instead of implying one
		// command ran on both sides.
		if baseCommand == command {
			ev.Method = fmt.Sprintf("%s: %s", probeName, command)
		} else {
			ev.Method = fmt.Sprintf("%s: base=%s | candidate=%s", probeName, baseCommand, command)
		}
		evidences = append(evidences, ev)
	}

	agg := verdict.Aggregate(evidences)
	warnings = append(warnings, agg.Warnings...)
	unverif = append(unverif, agg.UnverifiedReason...)

	subject := bundle.Subject{RepoRoot: cfg.RepoRoot, BaseRef: cfg.BaseRef}
	candRef := pair.CandidateCommit
	if pair.SnapshotRef != "" {
		candRef = pair.SnapshotRef
	}
	subject.CandidateRef = &candRef
	subject.DiffSHA256 = bundle.DiffSHA256(diffBytes)
	if cfg.Intent != "" {
		intent := cfg.Intent
		subject.Intent = &intent
	}
	pb := bundle.NewProofBundle(bundleID, subject, profile, evidences, nil, agg.Verdict, baselineSummary(evidences))
	pb.UnverifiedClaims = unverif
	if err := pb.Validate(); err != nil {
		return Result{}, fmt.Errorf("bundle invalid: %w", err)
	}
	// Persist failure detail as blobs so `show`/hooks can explain a regression
	// without re-running anything (the parsers already produced these messages).
	logs := map[string][]byte{}
	for i := range pb.Evidence {
		ev := &pb.Evidence[i]
		if len(ev.Failures) == 0 {
			continue
		}
		var sb strings.Builder
		for _, f := range ev.Failures {
			fmt.Fprintf(&sb, "%s\n    %s\n", f.ID, f.Message)
		}
		hash := bundle.DiffSHA256([]byte(ev.ID))
		name := "sha256-" + hash[len("sha256:"):len("sha256:")+12] + "/" + ev.Probe + "-candidate.log"
		logs[name] = []byte(sb.String())
		ev.Logs = "blobs/" + name
	}
	storeDir := bundle.NewStore(cfg.RepoRoot)
	if _, err := storeDir.Save(pb, logs); err != nil {
		return Result{}, fmt.Errorf("persist bundle: %w", err)
	}
	// Bound both the evidence store and the snapshot namespace on every run:
	// keeping every bundle ever produced would grow verify's fixed cost and pin
	// a full working-tree snapshot per run. `vouch gc` remains for manual runs.
	pruneBundles(cfg.RepoRoot, BundleKeep)
	_, _ = worktree.PruneSnapshotRefsExcept(cfg.RepoRoot, SnapshotRefKeep, storedSnapshotRefs(cfg.RepoRoot))

	return Result{
		Profile:         profile,
		Selection:       sel,
		Evidences:       evidences,
		Warnings:        warnings,
		Unverified:      unverif,
		Verdict:         agg.Verdict,
		Bundle:          pb,
		SnapshotRef:     pair.SnapshotRef,
		CandidateCommit: pair.CandidateCommit,
	}, nil
}

// DefaultBudgetMs is the total wall-clock budget of one verify run.
const DefaultBudgetMs = 600_000

// MaxBudgetMs bounds a caller-supplied budget. Beyond it the Duration
// conversion (ms → ns) would overflow int64 and context.WithTimeout would
// expire immediately; 24h is far past any real verification.
const MaxBudgetMs = 24 * 60 * 60 * 1000

// remainingBudgetMs reports how much of the run's total wall-clock budget is
// still left. No deadline (tests, direct library use) means "unbounded".
func remainingBudgetMs(ctx context.Context) int {
	dl, ok := ctx.Deadline()
	if !ok {
		return MaxBudgetMs
	}
	left := time.Until(dl)
	if left <= 0 {
		return 0
	}
	ms := left.Milliseconds()
	if ms <= 0 {
		return 1
	}
	if ms > MaxBudgetMs {
		return MaxBudgetMs
	}
	return int(ms)
}

// probeBudget returns the deadline for ONE probe command: the probe's own
// budget (ProbeBudgetMs, else the 120s default) capped by what remains of the
// run's TOTAL budget (Config.BudgetMs). The cap is recomputed per probe, so a
// slow first probe cannot leave a later command running past the total
// deadline. Zero means the total budget is already spent: the caller must not
// launch the command at all (a zero budget would be "inconclusive" anyway) and
// must record the probe as trimmed in unverified_claims.
func probeBudget(ctx context.Context, cfg Config, probe string) int {
	budget := 120_000
	if cfg.ProbeBudgetMs != nil {
		if v, ok := cfg.ProbeBudgetMs[probe]; ok && v > 0 {
			budget = v
		}
	}
	if budget > MaxBudgetMs {
		budget = MaxBudgetMs
	}
	remaining := remainingBudgetMs(ctx)
	if remaining <= 0 {
		return 0
	}
	if budget > remaining {
		budget = remaining
	}
	return budget
}

func runBuiltin(ctx context.Context, name, workdir, role, command string, budgetMs int) probe.RunResult {
	for _, r := range builtin.All() {
		if r.Name() != name {
			continue
		}
		inv, err := builtin.AsProbeRunner(r).Run(ctx, probe.RunParams{
			Workdir: workdir, Role: role, Command: command, BudgetMs: budgetMs,
		})
		if err != nil {
			return probe.Inconclusive(err.Error(), role)
		}
		return inv.Result
	}
	return probe.Inconclusive("unknown builtin probe "+name, role)
}

type cachedResult struct {
	Verdict string                 `json:"verdict"`
	Summary string                 `json:"summary"`
	Data    map[string]interface{} `json:"data"`
}

func cachedPayload(r probe.RunResult) cachedResult {
	return cachedResult{Verdict: string(r.Verdict), Summary: r.Summary, Data: r.Data}
}

func loadCached(store *cache.Store, cfg Config, probeName, env, targetsHash string) (probe.RunResult, bool) {
	if cfg.DisableCache {
		return probe.RunResult{}, false
	}
	var c cachedResult
	hit, err := store.Get(cache.Key{
		BaseRef: cfg.BaseRef, Probe: probeName, EnvFingerprint: env, TargetsHash: targetsHash,
	}, &c)
	if err != nil || !hit {
		return probe.RunResult{}, false
	}
	return probe.RunResult{Verdict: probe.Verdict(c.Verdict), Summary: c.Summary, Data: c.Data}, true
}

func baselineSummary(evs []bundle.Evidence) *bundle.BaselineSummary {
	if len(evs) == 0 {
		// Nothing ran: a baseline claim would be a lie ("all_green" with zero
		// evidence is exactly the failure mode §5 warns about).
		return nil
	}
	status := bundle.BaselineAllGreen
	failing := 0
	measured := 0
	for _, ev := range evs {
		// A probe that never produced a result (missing runner, crash, budget)
		// says nothing about the baseline: skip it instead of claiming "broken".
		if ev.Verdict == bundle.EvidenceInconclusive {
			continue
		}
		measured++
		failing += ev.Baseline.Fail
		switch ev.Baseline.Status {
		case bundle.BaselineExistingBroken:
			status = bundle.BaselineExistingBroken
		case bundle.BaselineExistingFailures:
			if status != bundle.BaselineExistingBroken {
				status = bundle.BaselineExistingFailures
			}
		}
	}
	if measured == 0 {
		return nil
	}
	return &bundle.BaselineSummary{Status: status, Failing: failing}
}

// EnvFingerprint collects the environment inputs for the cache key. Missing
// tools contribute an empty string (never omitted) so the key stays stable.
func EnvFingerprint(repoRoot string, profile bundle.ProjectProfile) string {
	goVer := toolVersion("go", "go version")
	nodeVer := toolVersion("node", "node -v")
	pnpmVer := toolVersion("pnpm", "pnpm -v")
	lockHash := lockFilesHash(repoRoot)
	return bundle.ProfileFingerprint(goVer, nodeVer, pnpmVer, lockHash, profile)
}

func toolVersion(bin, cmdline string) string {
	if _, err := exec.LookPath(bin); err != nil {
		return ""
	}
	parts := strings.Fields(cmdline)
	out, err := exec.Command(parts[0], parts[1:]...).Output()
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(out))
}

func lockFilesHash(repoRoot string) string {
	var names []string
	for _, n := range []string{"go.sum", "package-lock.json", "pnpm-lock.yaml", "yarn.lock", "poetry.lock", "uv.lock", "Cargo.lock"} {
		p := filepath.Join(repoRoot, n)
		if _, err := os.Stat(p); err == nil {
			names = append(names, n)
		}
	}
	sort.Strings(names)
	h := strings.Builder{}
	for _, n := range names {
		if b, err := os.ReadFile(filepath.Join(repoRoot, n)); err == nil {
			h.WriteString(n)
			h.WriteByte(0)
			h.Write(b)
			h.WriteByte(0)
		}
	}
	return bundle.DiffSHA256([]byte(h.String()))
}

// SnapshotRefKeep is how many candidate snapshot refs verify retains.
const SnapshotRefKeep = 20

// BundleKeep is how many recent bundles verify retains before LRU pruning.
const BundleKeep = 50

// setupBudgetResult converts a setup failure caused by the run's expired total
// budget into an UNVERIFIED Result instead of a tool error.
//
// The cause cannot be read off the error alone: a git process that was already
// running when exec.CommandContext killed it reports *exec.ExitError
// ("signal: killed"), not context.DeadlineExceeded. The branch is therefore on
// ctx.Err(), which covers both "the deadline had already expired before Start"
// and "the process was killed after Start". A healthy context means a real
// setup failure (bad ref, no repository), and the caller must keep the raw
// error.
//
// Stage, context reason and configured budget are carried into
// unverified_claims, so a report can never say "nothing was measured" without
// saying why. The bundle is still schema-valid: low confidence with an empty
// command set is the honest description of "setup never completed".
func setupBudgetResult(ctx context.Context, cfg Config, stage string, err error) (Result, bool) {
	if ctx.Err() == nil {
		return Result{}, false
	}
	cause := "budget exhausted"
	if errors.Is(ctx.Err(), context.Canceled) {
		cause = "run canceled"
	}
	claim := fmt.Sprintf("%s during setup (%s): %v (configured total budget %dms)", cause, stage, ctx.Err(), cfg.BudgetMs)
	if err != nil && !errors.Is(err, ctx.Err()) {
		claim += ": " + err.Error()
	}
	profile := bundle.ProjectProfile{
		Language:   []string{},
		Commands:   map[string]bundle.Command{},
		Confidence: bundle.ConfidenceLow,
		Gaps:       []string{claim},
	}
	// No content address is fabricated here: BundleIDFor(nil, baseRef) would
	// collide with a genuine "no changes" run and Save would atomically replace
	// that evidence (show/rerun would lose it). The result stays in-memory.
	return Result{
		Profile:    profile,
		Unverified: []string{claim},
		Verdict:    bundle.Unverified,
	}, true
}

// resolveCommit turns a ref into a concrete commit SHA. It runs under the
// run's total-budget ctx so a git call cannot outlive the deadline.
func resolveCommit(ctx context.Context, repoRoot, ref string) (string, error) {
	cmd := exec.CommandContext(ctx, "git", "rev-parse", "--verify", ref+"^{commit}")
	cmd.Dir = repoRoot
	out, err := cmd.Output()
	if err != nil {
		return "", err
	}
	return strings.TrimSpace(string(out)), nil
}

// DiffBetween returns the diff and changed-file list from baseRef to the
// candidate snapshot (or to the working tree when the snapshot is empty).
// Including the snapshot is what makes untracked additions visible.
func DiffBetween(repoRoot, baseRef, snapshot string) ([]byte, []string) {
	target := snapshot
	if target == "" {
		target = "HEAD"
	}
	bytesCmd := exec.Command("git", "diff", "--binary", baseRef, target, "--", ".", ":(exclude).vouch")
	bytesCmd.Dir = repoRoot
	bytesOut, err := bytesCmd.Output()
	if err != nil {
		bytesOut = nil
	}
	filesCmd := exec.Command("git", "diff", "--name-only", baseRef, target, "--", ".", ":(exclude).vouch")
	filesCmd.Dir = repoRoot
	filesOut, err := filesCmd.Output()
	if err != nil {
		return bytesOut, nil
	}
	var files []string
	for _, line := range strings.Split(string(filesOut), "\n") {
		if s := strings.TrimSpace(line); s != "" {
			files = append(files, s)
		}
	}
	return bytesOut, files
}

// filterSelfArtifacts drops vouch's own artifacts from the diff view: a cache
// file we wrote must never be treated as a change under review (it would both
// pollute the selector and change the bundle identity).
func filterSelfArtifacts(files []string) []string {
	out := make([]string, 0, len(files))
	for _, f := range files {
		if f == ".vouch" || strings.HasPrefix(f, ".vouch/") {
			continue
		}
		out = append(out, f)
	}
	return out
}

// bootstrapDeps closes the "fresh clone" gap: a JS checkout without
// node_modules cannot run its runner, which used to surface as an
// unexplained UNVERIFIED. Install is best-effort and bounded; failures become
// profile gaps (visible in --explain-gaps), never silent.
// Python is intentionally not auto-installed: mutating the user's interpreter
// is worse than an explicit gap.
func bootstrapDeps(ctx context.Context, cfg Config, profile bundle.ProjectProfile) []string {
	var gaps []string
	pm := ""
	if profile.PackageManager != nil {
		pm = *profile.PackageManager
	}
	isJS := pm == "npm" || pm == "pnpm" || pm == "yarn" || pm == "bun"
	if isJS {
		if _, err := os.Stat(filepath.Join(cfg.RepoRoot, "node_modules")); err != nil {
			if !cfg.InstallDeps {
				gaps = append(gaps, "node_modules missing: run `"+pm+" install` first, or pass --install-deps (verify does not install by default)")
			} else {
				before := trackedChanges(cfg.RepoRoot)
				beforeUntracked := untrackedFiles(cfg.RepoRoot)
				budget := cfg.DepsBudgetMs
				if budget <= 0 {
					budget = 300_000
				}
				installCtx, cancel := context.WithTimeout(ctx, time.Duration(budget)*time.Millisecond)
				defer cancel()
				cmdline := map[string]string{
					"npm":  "npm install --no-audit --no-fund",
					"pnpm": "pnpm install",
					"yarn": "yarn install",
					"bun":  "bun install",
				}[pm]
				// Install needs the network, so it declares it explicitly instead
				// of bypassing the sandbox contract.
				res, err := sandbox.Run(installCtx, cfg.RepoRoot, cmdline, sandbox.Policy{NeedsNetwork: true, Timeout: time.Duration(budget) * time.Millisecond})
				if err != nil || res.ExitCode != 0 {
					reason := ""
					if err != nil {
						reason = err.Error()
					} else {
						reason = lastLine(res.Stderr)
					}
					gaps = append(gaps, fmt.Sprintf("dependency install failed (%s): %s", cmdline, reason))
				} else {
					// Installing may rewrite tracked lockfiles or create new ones.
					// Restore the tracked ones and locally ignore the created ones,
					// so the tool's own action never becomes part of the reviewed
					// diff (which would change the bundle identity per machine).
					changed := diffTrackedChanges(before, trackedChanges(cfg.RepoRoot))
					for _, f := range changed {
						restore := exec.Command("git", "checkout", "--", f)
						restore.Dir = cfg.RepoRoot
						if err := restore.Run(); err != nil {
							gaps = append(gaps, "could not restore file modified by dependency install: "+f)
						}
					}
					created := diffTrackedChanges(beforeUntracked, untrackedFiles(cfg.RepoRoot))
					var ignored, reported []string
					for _, f := range created {
						if isDependencyArtifact(f) {
							ignored = append(ignored, f)
							continue
						}
						reported = append(reported, f)
					}
					if len(ignored) > 0 {
						if err := gitexclude.Add(cfg.RepoRoot, ignored...); err != nil {
							gaps = append(gaps, "dependency artifacts could not be ignored: "+strings.Join(ignored, ", "))
						}
					}
					if len(reported) > 0 {
						gaps = append(gaps, "dependency install created files (left visible for you to decide): "+strings.Join(reported, ", "))
					}
					if len(changed) > 0 {
						gaps = append(gaps, "dependency install rewrote tracked files (restored): "+strings.Join(changed, ", "))
					}
				}
			}
		}
	}
	if isPythonProfile(profile) {
		if err := exec.Command("python3", "-m", "pytest", "--version").Run(); err != nil {
			if _, lookErr := exec.LookPath("pytest"); lookErr != nil {
				// Prerequisite, not a bug: declared in README (see Prerequisites).
				gaps = append(gaps, "pytest not available (prerequisite): run `python3 -m pip install pytest`")
			}
		}
	}
	return gaps
}

func isPythonProfile(p bundle.ProjectProfile) bool {
	for _, l := range p.Language {
		if l == "python" {
			return true
		}
	}
	return false
}

func lastLine(s string) string {
	lines := strings.Split(strings.TrimSpace(s), "\n")
	if len(lines) == 0 {
		return ""
	}
	line := lines[len(lines)-1]
	if len(line) > 200 {
		line = line[:200]
	}
	return line
}

// pruneBundles deletes the oldest bundles beyond keep (LRU by bundle.json mtime).
func pruneBundles(repoRoot string, keep int) {
	store := bundle.NewStore(repoRoot)
	metas, err := store.List()
	if err != nil || len(metas) <= keep {
		return
	}
	sort.Slice(metas, func(i, j int) bool { return metas[i].SavedAt.After(metas[j].SavedAt) })
	for i, m := range metas {
		if i < keep {
			continue
		}
		_ = os.RemoveAll(filepath.Join(repoRoot, ".vouch", "bundles", m.BundleID))
	}
}

// storedSnapshotRefs returns the candidate refs referenced by stored bundles.
func storedSnapshotRefs(repoRoot string) map[string]bool {
	out := map[string]bool{}
	store := bundle.NewStore(repoRoot)
	metas, err := store.List()
	if err != nil {
		return out
	}
	// Only the newest BundleKeep bundles can still exist after pruning above,
	// so only they need protection (avoids parsing the whole history each run).
	if len(metas) > BundleKeep {
		sort.Slice(metas, func(i, j int) bool { return metas[i].SavedAt.After(metas[j].SavedAt) })
		metas = metas[:BundleKeep]
	}
	for _, m := range metas {
		b, err := store.Load(m.BundleID)
		if err != nil || b.Subject.CandidateRef == nil {
			continue
		}
		out[*b.Subject.CandidateRef] = true
	}
	return out
}

// emptyNarrowedRun reports whether a narrowed test run measured no cases at all
// (no test files matched, or the runner reported "no tests found").
func emptyNarrowedRun(base, cand probe.RunResult) bool {
	// Neither side measured a case AND the runner actually produced a result:
	// the narrowing matched no tests (vitest/jest print "No test files found"
	// and exit 1, so the probe reports a conclusive verdict with zero cases).
	// A missing runner is inconclusive on both sides — re-running it would only
	// burn budget and would mislabel the cause in the bundle.
	conclusive := false
	noTestsSignal := false
	for _, r := range []probe.RunResult{base, cand} {
		f := probe.TestFactsOf(r)
		if len(f.Passed)+len(f.Failed)+len(f.Skipped) > 0 {
			return false
		}
		if r.Verdict != probe.VerdictInconclusive {
			conclusive = true
		}
		if strings.Contains(strings.ToLower(r.Reason), "collected no tests") {
			noTestsSignal = true
		}
	}
	// Fall back when a side produced a conclusive "zero cases" result, or when
	// the probe explicitly reported "runner collected no tests" (inconclusive
	// because that runner has no machine-readable reporter). A missing runner
	// reports "runner not available" and must not trigger a pointless rerun.
	return conclusive || noTestsSignal
}

// trackedChanges lists tracked files with pending modifications.
func trackedChanges(repoRoot string) map[string]bool {
	out := map[string]bool{}
	cmd := exec.Command("git", "diff", "--name-only")
	cmd.Dir = repoRoot
	data, err := cmd.Output()
	if err != nil {
		return out
	}
	for _, line := range strings.Split(string(data), "\n") {
		if f := strings.TrimSpace(line); f != "" {
			out[f] = true
		}
	}
	return out
}

func diffTrackedChanges(before, after map[string]bool) []string {
	var out []string
	for f := range after {
		if !before[f] {
			out = append(out, f)
		}
	}
	sort.Strings(out)
	return out
}

// untrackedFiles lists non-ignored untracked files.
func untrackedFiles(repoRoot string) map[string]bool {
	out := map[string]bool{}
	cmd := exec.Command("git", "ls-files", "--others", "--exclude-standard")
	cmd.Dir = repoRoot
	data, err := cmd.Output()
	if err != nil {
		return out
	}
	for _, line := range strings.Split(string(data), "\n") {
		if f := strings.TrimSpace(line); f != "" {
			out[f] = true
		}
	}
	return out
}

// isDependencyArtifact limits what --install-deps may hide: only dependency
// trees and lockfiles, never arbitrary files the installer happened to create
// (a blanket exclude would silently hide real project files from git forever).
func isDependencyArtifact(path string) bool {
	base := filepath.Base(path)
	if base == "node_modules" || strings.HasPrefix(filepath.ToSlash(path), "node_modules/") {
		return true
	}
	for _, name := range []string{"package-lock.json", "pnpm-lock.yaml", "yarn.lock", "bun.lockb", "npm-shrinkwrap.json"} {
		if base == name {
			return true
		}
	}
	return false
}

// budgetExhausted reports whether a probe was killed by its deadline rather
// than failing on its own merits.
func budgetExhausted(r probe.RunResult) bool {
	return r.Verdict == probe.VerdictInconclusive &&
		strings.Contains(strings.ToLower(r.Reason), "budget")
}

// inconclusiveReasonOf collects the distinct reasons the inconclusive sides of
// a probe gave. Both sides can report the same reason (e.g. a concurrent
// base/candidate pair killed by one deadline), and repeating it in the claim
// would only add noise.
func inconclusiveReasonOf(results ...probe.RunResult) string {
	seen := map[string]bool{}
	var reasons []string
	for _, r := range results {
		if r.Verdict != probe.VerdictInconclusive {
			continue
		}
		if s := strings.TrimSpace(r.Reason); s != "" && !seen[s] {
			seen[s] = true
			reasons = append(reasons, s)
		}
	}
	return strings.Join(reasons, "; ")
}

// mustEvidence panics only on invalid internal state; the values come from a
// controlled constructor path.
func mustEvidence(ev bundle.Evidence, err error) bundle.Evidence {
	if err != nil {
		panic(err)
	}
	return ev
}
