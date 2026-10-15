package loopoc

// Codec is one candidate encoding of the views table. It is the contract the
// candidate packages (blockz, cyclerle) implement and Measure drives.
type Codec interface {
	Name() string
	NewEncoder() Encoder
	// Open decodes records in emission order (with any rewritten records
	// already substituted). screens is the screens table of the durable
	// views: content stored outside the views bytes, which a decoder needs to
	// fill View.ScreenHash and to recompute elided chains.
	Open(records [][]byte, screens ScreenTable) (Decoder, error)
}

// Encoder receives views in seq order. It must never read ahead: every record
// it returns may depend only on views already appended.
type Encoder interface {
	Append(v View) error
	// Commit seals everything appended since the last Commit into zero or
	// more new immutable records (append-only; the store never rewrites).
	// A codec that wants to extend an open record instead returns it in
	// Rewritten with the index it replaces; the harness counts rewritten
	// bytes as write amplification and reports it separately.
	//
	// Indexes are global emission indexes (0 = the first record ever
	// returned). Views appended after the last Commit are not durable: a
	// crash loses them, and Open of the records so far must decode exactly
	// the committed views.
	Commit() (records [][]byte, rewritten map[int][]byte, err error)
}

// Decoder answers reader questions over the committed views.
type Decoder interface {
	Len() int64
	// At returns the view with this seq (1-based), full View incl. Chain.
	At(seq int64) (View, error)
	// SeekTime returns the seq of the view holding at atNs: the last view of
	// the segment with at_ns <= atNs (ties by larger seq), the same
	// semantics as visualstore.Reader.ViewAt. ErrNoView when there is none.
	SeekTime(segment int, atNs int64) (int64, error)
	// Range returns views fromSeq, fromSeq+1, ... up to n of them, fewer at
	// the end. 1 <= fromSeq <= Len()+1, n >= 0.
	Range(fromSeq int64, n int) ([]View, error)
}

// SizedEncoder is an optional Encoder side interface for codecs whose stored
// bytes are not the sum of their records (sqlite-v8 and other SQLite layouts,
// measured by dbstat pages). When implemented, Measure calls StoredBytes after
// the last Commit and uses it instead of Σ(len(record) + RecordOverhead); the
// records such a codec returns are then only its own locators (Result
// .ByteMethod says which method was used).
type SizedEncoder interface {
	Encoder
	StoredBytes() (int64, error)
	ByteMethod() string
}

// Encoders and decoders that hold resources may implement io.Closer; Measure
// closes the decoder first, then the encoder.

// RecordOverhead is the assumed fixed cost of storing one record (for example
// a row id and length prefix in a blob table): 16 bytes.
const RecordOverhead = 16
