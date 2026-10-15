package qualify

import (
	"bufio"
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"os"
	"path/filepath"
	"regexp"
	"slices"
)

// ScenarioSchema identifies a scenario fragment (scenarios/<id>/result.json).
const ScenarioSchema = "agent-reel/qualification-scenario/1"

// Metric kinds (plan decision 6 labels).
const (
	// KindMeasured is a direct observation.
	KindMeasured = "measured"
	// KindDerived is arithmetic over measured values within their measured
	// range; Formula and Inputs are required.
	KindDerived = "derived"
	// KindEstimate is any extrapolation or modelled value; Formula and
	// Inputs are required, and an estimate never passes a budget.
	KindEstimate = "estimate"
)

// Timeline labels of a scenario.
const (
	LabelAccelerated = "accelerated-logical"
	LabelRealtime    = "real-time"
)

// Check results.
const (
	CheckPass = "pass"
	CheckFail = "fail"
)

// Scenario statuses.
const (
	StatusComplete = "complete"
	StatusError    = "error"
)

// Scenario is one scenario fragment.
type Scenario struct {
	Schema  string `json:"schema"`
	ID      string `json:"id"`
	Profile string `json:"profile"`
	Status  string `json:"status"`
	Error   string `json:"error,omitempty"`
	// Label is the timeline label ("accelerated-logical" or "real-time")
	// plus any method label such as "process-cold; OS page cache not
	// dropped".
	Label     string    `json:"label"`
	LogicalNs int64     `json:"logicalNs,omitempty"`
	WallNs    int64     `json:"wallNs,omitempty"`
	Host      *Host     `json:"host,omitempty"`
	Cells     []Cell    `json:"cells"`
	Checks    []Check   `json:"checks"`
	Metrics   []Metric  `json:"metrics,omitempty"`
	Findings  []Finding `json:"findings,omitempty"`
}

// Host is the host context around one scenario.
type Host struct {
	LoadStart string `json:"loadStart"`
	LoadEnd   string `json:"loadEnd"`
	FSType    string `json:"fsType"`
}

// Cell is one workload at one target count and timeline.
type Cell struct {
	Workload string   `json:"workload"`
	Targets  int      `json:"targets"`
	Timeline string   `json:"timeline"`
	Geometry string   `json:"geometry,omitempty"`
	Metrics  []Metric `json:"metrics"`
	Checks   []Check  `json:"checks,omitempty"`
}

// Key is the cell key.
func (c Cell) Key() string { return CellKey(c.Workload, c.Targets, c.Timeline) }

// Metric is one labelled value. PerTarget, when present, holds one value per
// target (index = target number) and Value is their aggregate.
type Metric struct {
	ID         string             `json:"id"`
	Value      float64            `json:"value"`
	Unit       string             `json:"unit"`
	Kind       string             `json:"kind"`
	Method     string             `json:"method"`
	Formula    string             `json:"formula,omitempty"`
	Inputs     map[string]float64 `json:"inputs,omitempty"`
	PerTarget  []float64          `json:"perTarget,omitempty"`
	ReportOnly bool               `json:"reportOnly,omitempty"`
}

// Check is one correctness check.
type Check struct {
	ID          string `json:"id"`
	Description string `json:"description"`
	Result      string `json:"result"`
	Evidence    string `json:"evidence"`
}

// Finding is a product observation reported, not fixed (decision 12).
type Finding struct {
	ID             string `json:"id"`
	Evidence       string `json:"evidence"`
	SuspectedCause string `json:"suspectedCause"`
	ProposedRemedy string `json:"proposedRemedy"`
}

var scenarioID = regexp.MustCompile(`^[ABC]\.[a-z0-9-]+(\.[A-Za-z0-9-]+)*$`)

// Validate refuses a malformed metric. Every metric carries a kind and a
// method; derived and estimate metrics also need Formula and Inputs.
func (m Metric) Validate() error {
	if !metricID.MatchString(m.ID) {
		return fmt.Errorf("metric id %q", m.ID)
	}
	if _, err := parseUnit(m.Unit); err != nil {
		return fmt.Errorf("metric %s: %w", m.ID, err)
	}
	switch m.Kind {
	case KindMeasured:
	case KindDerived, KindEstimate:
		if m.Formula == "" || len(m.Inputs) == 0 {
			return fmt.Errorf("metric %s: a %s metric needs a formula and inputs", m.ID, m.Kind)
		}
	case "":
		return fmt.Errorf("metric %s: kind is required", m.ID)
	default:
		return fmt.Errorf("metric %s: kind %q", m.ID, m.Kind)
	}
	if m.Method == "" {
		return fmt.Errorf("metric %s: method is required", m.ID)
	}
	if !finite(m.Value) {
		return fmt.Errorf("metric %s: value %v", m.ID, m.Value)
	}
	for _, v := range m.PerTarget {
		if !finite(v) {
			return fmt.Errorf("metric %s: per-target value %v", m.ID, v)
		}
	}
	for k, v := range m.Inputs {
		if k == "" || !finite(v) {
			return fmt.Errorf("metric %s: input %q=%v", m.ID, k, v)
		}
	}
	return nil
}

func finite(v float64) bool { return !math.IsNaN(v) && !math.IsInf(v, 0) }

func validateMetrics(where string, ms []Metric) error {
	seen := map[string]bool{}
	for _, m := range ms {
		if err := m.Validate(); err != nil {
			return fmt.Errorf("%s: %w", where, err)
		}
		if seen[m.ID] {
			return fmt.Errorf("%s: duplicate metric %s", where, m.ID)
		}
		seen[m.ID] = true
	}
	return nil
}

func validateChecks(where string, cs []Check) error {
	seen := map[string]bool{}
	for _, c := range cs {
		if c.ID == "" || c.Description == "" {
			return fmt.Errorf("%s: check without id/description", where)
		}
		if c.Result != CheckPass && c.Result != CheckFail {
			return fmt.Errorf("%s: check %s result %q", where, c.ID, c.Result)
		}
		if seen[c.ID] {
			return fmt.Errorf("%s: duplicate check %s", where, c.ID)
		}
		seen[c.ID] = true
	}
	return nil
}

// Validate refuses a malformed scenario fragment.
func (s Scenario) Validate() error {
	if s.Schema != ScenarioSchema {
		return fmt.Errorf("scenario schema %q", s.Schema)
	}
	if !scenarioID.MatchString(s.ID) {
		return fmt.Errorf("scenario id %q", s.ID)
	}
	if s.Profile != ProfileSmoke && s.Profile != ProfileFull {
		return fmt.Errorf("scenario %s: profile %q", s.ID, s.Profile)
	}
	switch s.Status {
	case StatusComplete:
	case StatusError:
		if s.Error == "" {
			return fmt.Errorf("scenario %s: error status without error text", s.ID)
		}
	default:
		return fmt.Errorf("scenario %s: status %q", s.ID, s.Status)
	}
	if s.Label == "" {
		return fmt.Errorf("scenario %s: label is required", s.ID)
	}
	keys := map[string]bool{}
	for _, c := range s.Cells {
		if c.Workload == "" || c.Targets < 1 || c.Timeline == "" {
			return fmt.Errorf("scenario %s: cell %+v", s.ID, c)
		}
		if keys[c.Key()] {
			return fmt.Errorf("scenario %s: duplicate cell %s", s.ID, c.Key())
		}
		keys[c.Key()] = true
		if err := validateMetrics(s.ID+"/"+c.Key(), c.Metrics); err != nil {
			return err
		}
		for _, m := range c.Metrics {
			if len(m.PerTarget) > 0 && len(m.PerTarget) != c.Targets {
				return fmt.Errorf("scenario %s/%s: metric %s has %d per-target values for %d targets", s.ID, c.Key(), m.ID, len(m.PerTarget), c.Targets)
			}
		}
		if err := validateChecks(s.ID+"/"+c.Key(), c.Checks); err != nil {
			return err
		}
	}
	if err := validateMetrics(s.ID, s.Metrics); err != nil {
		return err
	}
	if err := validateChecks(s.ID, s.Checks); err != nil {
		return err
	}
	for _, f := range s.Findings {
		if f.ID == "" || f.Evidence == "" {
			return fmt.Errorf("scenario %s: finding without id/evidence", s.ID)
		}
	}
	return nil
}

// WriteScenario validates s and writes <dir>/scenarios/<id>/result.json.
func WriteScenario(dir string, s Scenario) error {
	if s.Schema == "" {
		s.Schema = ScenarioSchema
	}
	if err := s.Validate(); err != nil {
		return err
	}
	sd := filepath.Join(dir, "scenarios", s.ID)
	if err := os.MkdirAll(sd, 0o700); err != nil {
		return err
	}
	b, err := json.MarshalIndent(s, "", "  ")
	if err != nil {
		return err
	}
	return writeFileAtomic(filepath.Join(sd, "result.json"), append(b, '\n'))
}

// ReadScenario strictly decodes and validates one fragment.
func ReadScenario(path string) (Scenario, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return Scenario{}, err
	}
	var s Scenario
	dec := json.NewDecoder(bytes.NewReader(data))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&s); err != nil {
		return Scenario{}, fmt.Errorf("%s: %w", path, err)
	}
	if _, err := dec.Token(); !errors.Is(err, io.EOF) {
		return Scenario{}, fmt.Errorf("%s: trailing data", path)
	}
	if err := s.Validate(); err != nil {
		return Scenario{}, fmt.Errorf("%s: %w", path, err)
	}
	return s, nil
}

func writeFileAtomic(path string, data []byte) error {
	tmp, err := os.CreateTemp(filepath.Dir(path), "."+filepath.Base(path)+".*.tmp")
	if err != nil {
		return err
	}
	name := tmp.Name()
	if _, err := tmp.Write(data); err != nil {
		tmp.Close()
		os.Remove(name)
		return err
	}
	if err := tmp.Close(); err != nil {
		os.Remove(name)
		return err
	}
	if err := os.Chmod(name, 0o600); err != nil {
		os.Remove(name)
		return err
	}
	return os.Rename(name, path)
}

// Sample is one line of samples.jsonl: a named set of values for one target
// (-1 for the aggregate) at a logical time and a wall time.
type Sample struct {
	Target    int                `json:"target"`
	LogicalNs int64              `json:"logicalNs"`
	WallNs    int64              `json:"wallNs"`
	Values    map[string]float64 `json:"values"`
}

// SampleWriter appends samples as JSON lines.
type SampleWriter struct {
	f   *os.File
	w   *bufio.Writer
	enc *json.Encoder
}

// CreateSamples creates <path> (it must not exist).
func CreateSamples(path string) (*SampleWriter, error) {
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		return nil, err
	}
	w := bufio.NewWriter(f)
	return &SampleWriter{f: f, w: w, enc: json.NewEncoder(w)}, nil
}

// Write appends one sample.
func (s *SampleWriter) Write(sample Sample) error {
	for k, v := range sample.Values {
		if !finite(v) {
			return fmt.Errorf("qualify: sample value %s=%v", k, v)
		}
	}
	return s.enc.Encode(sample)
}

// Close flushes and closes the file.
func (s *SampleWriter) Close() error {
	err := s.w.Flush()
	if cerr := s.f.Close(); err == nil {
		err = cerr
	}
	return err
}

// ReadSamples reads every sample of a samples.jsonl file.
func ReadSamples(path string) ([]Sample, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	var out []Sample
	dec := json.NewDecoder(bufio.NewReader(f))
	dec.DisallowUnknownFields()
	for {
		var s Sample
		if err := dec.Decode(&s); errors.Is(err, io.EOF) {
			return out, nil
		} else if err != nil {
			return out, err
		}
		out = append(out, s)
	}
}

// SortedKeys returns m's keys in order.
func sortedKeys[V any](m map[string]V) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	slices.Sort(keys)
	return keys
}
