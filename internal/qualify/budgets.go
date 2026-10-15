package qualify

import (
	"bytes"
	"crypto/sha256"
	_ "embed"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"regexp"
	"slices"
	"strings"
)

// BudgetsSchema identifies the frozen budgets file format.
const BudgetsSchema = "agent-reel/qualification-budgets/1"

//go:embed budgets.json
var budgetsJSON []byte

// BudgetsJSON returns a copy of the embedded, frozen budgets file.
func BudgetsJSON() []byte { return bytes.Clone(budgetsJSON) }

// BudgetsSHA256 is the hex sha256 of the embedded budgets file.
func BudgetsSHA256() string { return sha256Hex(budgetsJSON) }

func sha256Hex(b []byte) string {
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:])
}

// BudgetFile is the decoded budgets file.
type BudgetFile struct {
	Schema     string      `json:"schema"`
	Note       string      `json:"note"`
	Budgets    []Budget    `json:"budgets"`
	Amendments []Amendment `json:"amendments"`
}

// Budget is one row of the plan's budgets table. Metric, Budget and
// Basis.Cite are the table's text verbatim; Limits encode its values.
type Budget struct {
	ID     string  `json:"id"`
	Metric string  `json:"metric"`
	Budget string  `json:"budget"`
	Scope  Scope   `json:"scope"`
	Limits []Limit `json:"limits"`
	Basis  Basis   `json:"basis"`
}

// Scope selects the scenario cells a budget is judged on. Empty Workloads,
// Targets or Timelines mean any; Scenarios ["*"] means every scenario.
type Scope struct {
	Tier             string   `json:"tier"`
	Scenarios        []string `json:"scenarios"`
	Workloads        []string `json:"workloads"`
	Targets          []int    `json:"targets"`
	Timelines        []string `json:"timelines"`
	Profile          string   `json:"profile"`
	ExcludeWorkloads []string `json:"excludeWorkloads,omitempty"`
}

// Limit is one comparison. The threshold is
//
//	max(Σ factor × term, floor) + value   (floor only when set)
//	Σ factor × term + value               (otherwise; value alone without terms)
//
// in the limit's unit. PerTarget limits must hold for every target.
type Limit struct {
	Key        string   `json:"key"`
	Metric     string   `json:"metric"`
	PerTarget  bool     `json:"perTarget"`
	Comparator string   `json:"comparator"`
	Value      float64  `json:"value"`
	Unit       string   `json:"unit"`
	BasisKind  string   `json:"basisKind"`
	Workloads  []string `json:"workloads,omitempty"`
	Timelines  []string `json:"timelines,omitempty"`
	Terms      []Term   `json:"terms,omitempty"`
	Floor      *float64 `json:"floor,omitempty"`
}

// Term references another metric of the same cell. Unit is the unit the
// referenced metric must be convertible to; Factor maps it into the limit's
// unit.
type Term struct {
	Metric string  `json:"metric"`
	Factor float64 `json:"factor"`
	Unit   string  `json:"unit"`
}

// Basis says where a budget comes from.
type Basis struct {
	Kinds      []string `json:"kinds"`
	Cite       string   `json:"cite"`
	Derivation string   `json:"derivation"`
}

// Amendment records a user-approved change to a frozen budget.
type Amendment struct {
	Date       string `json:"date"`
	ID         string `json:"id"`
	Old        string `json:"old"`
	New        string `json:"new"`
	Reason     string `json:"reason"`
	ApprovedBy string `json:"approvedBy"`
}

// Basis kinds.
const (
	BasisMeasuredPrior  = "measured-prior"
	BasisRequirement    = "requirement"
	BasisSchemaEstimate = "schema-estimate"
)

var (
	budgetID    = regexp.MustCompile(`^B[0-9]{2}[a-z]?$`)
	metricID    = regexp.MustCompile(`^[a-z0-9_]+(\.[a-z0-9_]+)*$`)
	comparators = []string{"<=", ">=", "=="}
	basisKinds  = []string{BasisMeasuredPrior, BasisRequirement, BasisSchemaEstimate}
)

// LoadBudgets returns the embedded, frozen budgets.
func LoadBudgets() (BudgetFile, error) { return DecodeBudgets(budgetsJSON) }

// DecodeBudgets strictly decodes and validates a budgets file: unknown fields,
// trailing data, duplicate IDs and invalid entries are refused.
func DecodeBudgets(data []byte) (BudgetFile, error) {
	var f BudgetFile
	dec := json.NewDecoder(bytes.NewReader(data))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&f); err != nil {
		return BudgetFile{}, fmt.Errorf("qualify: budgets: %w", err)
	}
	if _, err := dec.Token(); !errors.Is(err, io.EOF) {
		return BudgetFile{}, fmt.Errorf("qualify: budgets: trailing data")
	}
	if err := f.Validate(); err != nil {
		return BudgetFile{}, err
	}
	return f, nil
}

// Validate checks the structure of a budgets file.
func (f BudgetFile) Validate() error {
	if f.Schema != BudgetsSchema {
		return fmt.Errorf("qualify: budgets schema %q", f.Schema)
	}
	if len(f.Budgets) == 0 {
		return fmt.Errorf("qualify: budgets: none")
	}
	if f.Amendments == nil {
		return fmt.Errorf("qualify: budgets: amendments must be present (possibly empty)")
	}
	known := map[string]bool{}
	for _, s := range PlannedScenarios(ProfileFull) {
		known[s] = true
	}
	ids, keys := map[string]bool{}, map[string]bool{}
	for _, b := range f.Budgets {
		if !budgetID.MatchString(b.ID) {
			return fmt.Errorf("qualify: budget id %q", b.ID)
		}
		if ids[b.ID] {
			return fmt.Errorf("qualify: duplicate budget id %s", b.ID)
		}
		ids[b.ID] = true
		if strings.TrimSpace(b.Metric) == "" || strings.TrimSpace(b.Budget) == "" {
			return fmt.Errorf("qualify: budget %s: metric and budget text are required", b.ID)
		}
		if err := b.Scope.validate(known); err != nil {
			return fmt.Errorf("qualify: budget %s: %w", b.ID, err)
		}
		if len(b.Basis.Kinds) == 0 || strings.TrimSpace(b.Basis.Cite) == "" || strings.TrimSpace(b.Basis.Derivation) == "" {
			return fmt.Errorf("qualify: budget %s: basis kinds, cite and derivation are required", b.ID)
		}
		for i, k := range b.Basis.Kinds {
			if !slices.Contains(basisKinds, k) || slices.Contains(b.Basis.Kinds[:i], k) {
				return fmt.Errorf("qualify: budget %s: basis kind %q", b.ID, k)
			}
		}
		if len(b.Limits) == 0 {
			return fmt.Errorf("qualify: budget %s: no limits", b.ID)
		}
		for _, l := range b.Limits {
			if l.Key != b.ID && !strings.HasPrefix(l.Key, b.ID+".") {
				return fmt.Errorf("qualify: budget %s: limit key %q", b.ID, l.Key)
			}
			if keys[l.Key] {
				return fmt.Errorf("qualify: duplicate limit key %s", l.Key)
			}
			keys[l.Key] = true
			if err := l.validate(b); err != nil {
				return fmt.Errorf("qualify: limit %s: %w", l.Key, err)
			}
		}
	}
	for i, a := range f.Amendments {
		if a.Date == "" || a.ID == "" || a.Old == "" || a.New == "" || a.Reason == "" || a.ApprovedBy == "" {
			return fmt.Errorf("qualify: amendment %d: every field is required", i)
		}
		if !ids[a.ID] && !keys[a.ID] {
			return fmt.Errorf("qualify: amendment %d names unknown budget %q", i, a.ID)
		}
	}
	return nil
}

func (s Scope) validate(known map[string]bool) error {
	switch s.Tier {
	case "A", "B", "C", "any":
	default:
		return fmt.Errorf("scope tier %q", s.Tier)
	}
	if s.Profile != ProfileFull {
		return fmt.Errorf("scope profile %q (budgets are declared for the full profile)", s.Profile)
	}
	if len(s.Scenarios) == 0 {
		return fmt.Errorf("scope has no scenarios")
	}
	for _, id := range s.Scenarios {
		if id == "*" {
			if len(s.Scenarios) != 1 {
				return fmt.Errorf("scope mixes * with named scenarios")
			}
			continue
		}
		if !known[id] {
			return fmt.Errorf("scope names unplanned scenario %q", id)
		}
		if s.Tier != "any" && !strings.HasPrefix(id, s.Tier+".") {
			return fmt.Errorf("scenario %q is outside tier %s", id, s.Tier)
		}
	}
	for _, t := range s.Targets {
		if t < 1 {
			return fmt.Errorf("scope target count %d", t)
		}
	}
	return nil
}

func (l Limit) validate(b Budget) error {
	if !metricID.MatchString(l.Metric) {
		return fmt.Errorf("metric id %q", l.Metric)
	}
	if !slices.Contains(comparators, l.Comparator) {
		return fmt.Errorf("comparator %q", l.Comparator)
	}
	if math.IsNaN(l.Value) || math.IsInf(l.Value, 0) {
		return fmt.Errorf("value %v", l.Value)
	}
	if _, err := parseUnit(l.Unit); err != nil {
		return err
	}
	if !slices.Contains(b.Basis.Kinds, l.BasisKind) {
		return fmt.Errorf("basis kind %q is not among the budget's %v", l.BasisKind, b.Basis.Kinds)
	}
	for _, w := range l.Workloads {
		if len(b.Scope.Workloads) > 0 && !slices.Contains(b.Scope.Workloads, w) {
			return fmt.Errorf("workload %q is outside the budget scope", w)
		}
	}
	for _, tl := range l.Timelines {
		if len(b.Scope.Timelines) > 0 && !slices.Contains(b.Scope.Timelines, tl) {
			return fmt.Errorf("timeline %q is outside the budget scope", tl)
		}
	}
	for _, t := range l.Terms {
		if !metricID.MatchString(t.Metric) || t.Factor == 0 || math.IsNaN(t.Factor) || math.IsInf(t.Factor, 0) {
			return fmt.Errorf("term %+v", t)
		}
		if _, err := parseUnit(t.Unit); err != nil {
			return err
		}
	}
	if l.Floor != nil && len(l.Terms) == 0 {
		return fmt.Errorf("floor without terms")
	}
	return nil
}

// Budget returns the budget with id.
func (f BudgetFile) Budget(id string) (Budget, bool) {
	for _, b := range f.Budgets {
		if b.ID == id {
			return b, true
		}
	}
	return Budget{}, false
}

// ---------------------------------------------------------------------------
// Units: "num" or "num/den". Byte and time numerators convert within their
// family; logical-time and time denominators convert within theirs; every
// other name must match exactly.

type unit struct {
	numFamily, denFamily string
	numScale, denScale   float64
	num, den             string
}

var byteScale = map[string]float64{"B": 1, "KiB": 1 << 10, "MiB": 1 << 20, "GiB": 1 << 30}
var timeScale = map[string]float64{"ns": 1e-9, "us": 1e-6, "ms": 1e-3, "s": 1}
var logicalScale = map[string]float64{"logical-s": 1, "logical-min": 60, "logical-h": 3600, "logical-day": 86400}
var plainNum = []string{"count", "bool", "ratio", "percent", "cores", "CPU-s", "frames"}
var plainDen = []string{"view", "finding", "event"}

func parseUnit(u string) (unit, error) {
	num, den, _ := strings.Cut(u, "/")
	var out unit
	out.num, out.den = num, den
	switch {
	case byteScale[num] != 0:
		out.numFamily, out.numScale = "bytes", byteScale[num]
	case timeScale[num] != 0:
		out.numFamily, out.numScale = "time", timeScale[num]
	case slices.Contains(plainNum, num):
		out.numFamily, out.numScale = num, 1
	default:
		return unit{}, fmt.Errorf("unit %q", u)
	}
	switch {
	case den == "":
		out.denFamily, out.denScale = "", 1
	case logicalScale[den] != 0:
		out.denFamily, out.denScale = "logical", logicalScale[den]
	case timeScale[den] != 0:
		out.denFamily, out.denScale = "time", timeScale[den]
	case slices.Contains(plainDen, den):
		out.denFamily, out.denScale = den, 1
	default:
		return unit{}, fmt.Errorf("unit %q", u)
	}
	return out, nil
}

// ConvertUnit converts v from unit from to unit to.
func ConvertUnit(v float64, from, to string) (float64, error) {
	f, err := parseUnit(from)
	if err != nil {
		return 0, err
	}
	t, err := parseUnit(to)
	if err != nil {
		return 0, err
	}
	if f.numFamily != t.numFamily || f.denFamily != t.denFamily {
		return 0, fmt.Errorf("unit %q is not convertible to %q", from, to)
	}
	return v * f.numScale / t.numScale * t.denScale / f.denScale, nil
}

// GenesisBudgetsSHA256 is the sha256 of budgets.json as first frozen in V8
// Run 1. It is never edited: whenever the current budgets differ from it,
// amendments[] must be non-empty (TestBudgetsAmendedSinceGenesis, and a
// report correctness check), so editing the pinned hash alone cannot hide a
// change.
const GenesisBudgetsSHA256 = "fa9769b48857f6cf051dd46402498e2d4dbbc13d8491ea323f90340302ca4a36"

// FrozenBudgetsSHA256 is the pinned sha256 of budgets.json. Changing the file
// requires a user-approved amendments[] entry and editing this value and the
// literal in TestBudgetsFrozen.
const FrozenBudgetsSHA256 = "fa9769b48857f6cf051dd46402498e2d4dbbc13d8491ea323f90340302ca4a36"
