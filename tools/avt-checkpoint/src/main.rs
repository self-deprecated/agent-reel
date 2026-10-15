//! Framing only: all cast parsing, timeline and export policy remain in Go.
use std::io::{self, Read, Write};

const MAX_TEXT: usize = 4 << 20;
const MAX_DUMP: usize = 16 << 20;
// Visible readout: 32,768 cells at no more than 14 encoded bytes stays well below this.
const MAX_VIEW: usize = 1 << 20;

// Protocol handshake: op `P` replies with HELLO_MAGIC then PROTOCOL_VERSION as a
// big-endian u32. Bump the version whenever an operation is added or its reply
// changes, so an areel built against a different helper refuses it at session
// start instead of dying mid-capture. Version 1 was the start header plus
// F/R/D/Q; version 2 added the V/W visible readouts and P itself.
const HELLO_MAGIC: &[u8; 4] = b"AVTP";
const PROTOCOL_VERSION: u32 = 2;

// Scrollback the emulator RETAINS but never exports. The official player builds
// its own terminal with `create(cols, rows, 100, boldIsBright)`
// (asciinema-player 3.17.0, embedding avt 0.16.0; the vendored
// internal/reel/web/vendor/player.js). Matching that limit is what makes a
// row-growing resize restore the same lines here as it restores there.
const SCROLLBACK_LIMIT: usize = 100;

const ATTR_BOLD: u8 = 1;
const ATTR_FAINT: u8 = 1 << 1;
const ATTR_ITALIC: u8 = 1 << 2;
const ATTR_UNDERLINE: u8 = 1 << 3;
const ATTR_STRIKETHROUGH: u8 = 1 << 4;
const ATTR_BLINK: u8 = 1 << 5;
const ATTR_INVERSE: u8 = 1 << 6;

fn number(input: &mut impl Read) -> io::Result<usize> {
    let mut bytes = [0; 4];
    input.read_exact(&mut bytes)?;
    Ok(u32::from_be_bytes(bytes) as usize)
}
fn geometry(cols: usize, rows: usize) -> io::Result<()> {
    if cols == 0 || rows == 0 || cols > 512 || rows > 512 || cols * rows > 32768 {
        Err(io::Error::new(
            io::ErrorKind::InvalidData,
            "geometry exceeds bound",
        ))
    } else {
        Ok(())
    }
}

// Presentation encoding. Colors are exactly what avt models: absent (terminal
// default), palette index, or direct RGB. No dynamic palette is available.
fn encode_color(payload: &mut Vec<u8>, value: Option<avt::Color>) {
    match value {
        None => payload.push(0),
        Some(avt::Color::Indexed(index)) => payload.extend_from_slice(&[1, index]),
        Some(avt::Color::RGB(rgb)) => payload.extend_from_slice(&[2, rgb.r, rgb.g, rgb.b]),
    }
}
fn encode_cell(payload: &mut Vec<u8>, source: &avt::Cell) {
    payload.extend_from_slice(&(source.char() as u32).to_be_bytes());
    payload.push(source.width());
    let pen = source.pen();
    encode_color(payload, pen.foreground());
    encode_color(payload, pen.background());
    let mut attrs = 0;
    if pen.is_bold() {
        attrs |= ATTR_BOLD;
    }
    if pen.is_faint() {
        attrs |= ATTR_FAINT;
    }
    if pen.is_italic() {
        attrs |= ATTR_ITALIC;
    }
    if pen.is_underline() {
        attrs |= ATTR_UNDERLINE;
    }
    if pen.is_strikethrough() {
        attrs |= ATTR_STRIKETHROUGH;
    }
    if pen.is_blink() {
        attrs |= ATTR_BLINK;
    }
    if pen.is_inverse() {
        attrs |= ATTR_INVERSE;
    }
    payload.push(attrs);
}
fn bounded(payload: Vec<u8>, limit: usize) -> io::Result<Vec<u8>> {
    if payload.len() > limit {
        Err(io::Error::new(
            io::ErrorKind::InvalidData,
            "visible readout exceeds bound",
        ))
    } else {
        Ok(payload)
    }
}
// The cursor is reported with every readout; it is never a changed row.
fn visible_header(vt: &avt::Vt, output: &mut impl Write) -> io::Result<()> {
    let (cols, rows) = vt.size();
    let cursor = vt.cursor();
    output.write_all(&(cols as u32).to_be_bytes())?;
    output.write_all(&(rows as u32).to_be_bytes())?;
    output.write_all(&(cursor.col as u32).to_be_bytes())?;
    output.write_all(&(cursor.row as u32).to_be_bytes())?;
    output.write_all(&[u8::from(cursor.visible)])
}
// `view` is the visible grid of the ACTIVE buffer; no scrollback, no alternate
// flag. The emulator retains SCROLLBACK_LIMIT lines to mirror the official
// player, but neither `view` nor `changed` ever reads one: they report visible
// rows only.
fn view(vt: &avt::Vt, output: &mut impl Write, limit: usize) -> io::Result<()> {
    let mut payload = Vec::new();
    for line in vt.view() {
        for source in line.cells() {
            encode_cell(&mut payload, source);
        }
    }
    let payload = bounded(payload, limit)?;
    visible_header(vt, output)?;
    output.write_all(&(payload.len() as u32).to_be_bytes())?;
    output.write_all(&payload)?;
    output.flush()
}
fn changed(vt: &avt::Vt, dirty: &[bool], output: &mut impl Write, limit: usize) -> io::Result<()> {
    let mut payload = Vec::new();
    let mut count: u32 = 0;
    for (index, line) in vt.view().enumerate() {
        if !dirty.get(index).copied().unwrap_or(false) {
            continue;
        }
        count += 1;
        payload.extend_from_slice(&(index as u32).to_be_bytes());
        for source in line.cells() {
            encode_cell(&mut payload, source);
        }
    }
    let payload = bounded(payload, limit)?;
    visible_header(vt, output)?;
    output.write_all(&count.to_be_bytes())?;
    output.write_all(&payload)?;
    output.flush()
}

// The one production terminal builder; tests that assert production behaviour
// go through it so the configuration cannot drift. Scrollback is retained
// internally only, at the player's limit, and is never exported: `dump`, `view`
// and `changed` all read the visible grid alone. It reaches a readout consumer
// only through its effect on a later visible grid after a row-growing resize --
// exactly as it does in the player. The dump preserves current terminal state,
// including the inactive primary screen needed by an alternate-screen exit.
fn terminal(cols: usize, rows: usize) -> avt::Vt {
    avt::Vt::builder()
        .size(cols, rows)
        .scrollback_limit(SCROLLBACK_LIMIT)
        .build()
}

fn run(input: &mut impl Read, output: &mut impl Write) -> io::Result<()> {
    let mut magic = [0; 4];
    input.read_exact(&mut magic)?;
    if &magic != b"AVT1" {
        return Err(io::Error::new(
            io::ErrorKind::InvalidData,
            "invalid protocol",
        ));
    }
    let cols = number(input)?;
    let rows = number(input)?;
    geometry(cols, rows)?;
    let mut vt = terminal(cols, rows);
    // avt reports every row in its first `Changes`; the session already starts
    // with every row marked, so drain that baseline once and keep the accounting
    // honest afterwards.
    vt.feed_str("");
    // Union of every dirty row avt has reported since the last V/W readout.
    // A session starts with nothing observed, so every row counts as changed.
    let mut dirty = vec![true; rows];
    loop {
        let mut op = [0];
        input.read_exact(&mut op)?;
        match op[0] {
            b'F' => {
                let size = number(input)?;
                if size > MAX_TEXT {
                    return Err(io::Error::new(
                        io::ErrorKind::InvalidData,
                        "feed exceeds bound",
                    ));
                }
                let mut bytes = vec![0; size];
                input.read_exact(&mut bytes)?;
                let text = std::str::from_utf8(&bytes)
                    .map_err(|_| io::Error::new(io::ErrorKind::InvalidData, "invalid UTF-8"))?;
                for line in vt.feed_str(text).lines {
                    if line < dirty.len() {
                        dirty[line] = true;
                    }
                }
            }
            b'R' => {
                let cols = number(input)?;
                let rows = number(input)?;
                geometry(cols, rows)?;
                vt.resize(cols, rows);
                // Reflow can change every cell; avt reports that too, but the
                // readout contract does not depend on it.
                dirty.resize(rows, true);
                dirty.fill(true);
            }
            b'D' => {
                let dump = vt.dump();
                if dump.len() > MAX_DUMP {
                    return Err(io::Error::new(
                        io::ErrorKind::InvalidData,
                        "checkpoint exceeds bound",
                    ));
                }
                let (cols, rows) = vt.size();
                output.write_all(&(cols as u32).to_be_bytes())?;
                output.write_all(&(rows as u32).to_be_bytes())?;
                output.write_all(&(dump.len() as u32).to_be_bytes())?;
                output.write_all(dump.as_bytes())?;
                output.flush()?;
            }
            b'V' => {
                view(&vt, output, MAX_VIEW)?;
                dirty.fill(false);
            }
            b'W' => {
                changed(&vt, &dirty, output, MAX_VIEW)?;
                dirty.fill(false);
            }
            b'P' => {
                output.write_all(HELLO_MAGIC)?;
                output.write_all(&PROTOCOL_VERSION.to_be_bytes())?;
                output.flush()?;
            }
            b'Q' => return Ok(()),
            _ => {
                return Err(io::Error::new(
                    io::ErrorKind::InvalidData,
                    "unknown operation",
                ))
            }
        }
    }
}
fn main() {
    if let Err(error) = run(&mut io::stdin().lock(), &mut io::stdout().lock()) {
        eprintln!("avt checkpoint: {error}");
        std::process::exit(1);
    }
}

#[cfg(test)]
mod tests {
    use super::*;
    fn equivalent(prefix: &str, suffix: &str, resize: Option<(usize, usize)>) {
        let mut original = avt::Vt::builder().size(12, 5).scrollback_limit(0).build();
        original.feed_str(prefix);
        if let Some((cols, rows)) = resize {
            original.resize(cols, rows);
        }
        let (cols, rows) = original.size();
        let checkpoint = original.dump();
        let mut restored = avt::Vt::builder()
            .size(cols, rows)
            .scrollback_limit(0)
            .build();
        restored.feed_str(&checkpoint);
        assert_eq!(original.text(), restored.text());
        assert_eq!(original.cursor(), restored.cursor());
        original.feed_str(suffix);
        restored.feed_str(suffix);
        assert_eq!(original.text(), restored.text());
        assert_eq!(original.cursor(), restored.cursor());
        assert_eq!(original.dump(), restored.dump());
    }
    #[test]
    fn dump_preserves_continuation() {
        let cases = [
            ("\x1b[31m界 e\u{301}\r\nabc", "\x1b[0m日"),
            (
                "\x1b[?7labc\x1b[2;4r\x1b[?6h\x1b[3;4H\x1b7\x1b[31m",
                "\x1b8\tABCDEFGH\nX",
            ),
            ("\x1b[3g\x1b[5G\x1bH\r\x1b[4hABC", "\r\tX\x1b[4lY"),
            (
                "PRIMARY\x1b7\x1b[?1049hALTERNATE\x1b[?25l",
                "\x1b[?1049l\x1b8X",
            ),
            ("before\x1b[38;2;12;", "34;56mCOLOR"),
            ("before\x1b(", "0qqq\x1b(Bnormal"),
            ("before\x1b]0;ignored", " title\x07AFTER"),
            ("123456789012", "Z\r\nNEXT"),
        ];
        for (prefix, suffix) in cases {
            equivalent(prefix, suffix, None);
        }
        equivalent(
            "before\x1b[31m界\x1b[3;11HXYZ",
            "\x1b[2J\x1b[Hafter",
            Some((8, 7)),
        );
    }
    #[test]
    fn dump_excludes_erased_history() {
        let mut vt = avt::Vt::builder().size(80, 24).scrollback_limit(0).build();
        vt.feed_str("ERASED-SECRET\x1b[2J\x1b[Hvisible");
        assert!(!vt.dump().contains("ERASED-SECRET"));
        assert!(vt.dump().contains("visible"));
    }
    // The visible grid alone, so a test can tell it apart from `Vt::text`, which
    // walks scrollback as well.
    fn visible(vt: &avt::Vt) -> String {
        vt.view()
            .map(|line| line.text())
            .collect::<Vec<_>>()
            .join("\n")
    }
    fn numbered(vt: &mut avt::Vt, count: usize) {
        for n in 1..=count {
            vt.feed_str(&format!("line-{n:03}\r\n"));
        }
    }

    // Production configuration, built through `terminal` so this cannot drift
    // from it: rows growing after content scrolled off pulls that content back
    // into the visible grid, which is exactly what the official player does with
    // its own 100-line scrollback. Past the limit the lines are gone for good,
    // in both.
    #[test]
    fn production_scrollback_restores_lines_on_row_growth() {
        let mut vt = terminal(20, 4);
        numbered(&mut vt, 10);
        assert!(!visible(&vt).contains("line-001"), "{}", visible(&vt));
        assert!(visible(&vt).contains("line-010"), "{}", visible(&vt));
        // Seven lines scrolled off and all seven are inside the limit.
        vt.resize(20, 11);
        assert!(visible(&vt).contains("line-001"), "{}", visible(&vt));
        assert!(visible(&vt).contains("line-010"), "{}", visible(&vt));

        let mut capped = terminal(20, 4);
        numbered(&mut capped, SCROLLBACK_LIMIT + 10);
        // 107 lines scrolled off, so the oldest 7 were trimmed to the limit and
        // only the remaining 100 can come back.
        capped.resize(20, 200);
        let text = visible(&capped);
        assert!(!text.contains("line-001"), "{text}");
        assert!(!text.contains("line-007"), "{text}");
        assert!(text.contains("line-008"), "{text}");
        assert!(
            text.contains(&format!("line-{:03}", SCROLLBACK_LIMIT + 10)),
            "{text}"
        );
    }
    // Retaining scrollback does not put it in the checkpoint: the dump is the
    // visible state and nothing else, at the production limit as at zero.
    #[test]
    fn dump_excludes_scrollback() {
        let mut vt = terminal(20, 4);
        vt.feed_str("SCROLLED-SECRET\r\n");
        numbered(&mut vt, 10);
        let dump = vt.dump();
        assert!(!dump.contains("SCROLLED-SECRET"), "{dump}");
        assert!(!dump.contains("line-001"), "{dump}");
        assert!(dump.contains("line-010"), "{dump}");
    }
    #[test]
    fn protocol_bounds() {
        let mut input = b"AVT1".to_vec();
        input.extend_from_slice(&80u32.to_be_bytes());
        input.extend_from_slice(&24u32.to_be_bytes());
        input.push(b'F');
        input.extend_from_slice(&((MAX_TEXT + 1) as u32).to_be_bytes());
        assert!(run(&mut &input[..], &mut Vec::new()).is_err());
        assert!(geometry(513, 24).is_err());
        assert!(geometry(512, 512).is_err());
        assert!(geometry(0, 24).is_err());
    }

    // Readout decoding used only to assert the encoding this helper produces.
    fn cell_len(bytes: &[u8]) -> usize {
        let mut n = 5;
        for _ in 0..2 {
            n += match bytes[n] {
                0 => 1,
                1 => 2,
                2 => 4,
                kind => panic!("unknown colour kind {kind}"),
            };
        }
        n + 1
    }
    struct Readout {
        cols: usize,
        rows: usize,
        cursor: (usize, usize, bool),
        rows_changed: Vec<usize>,
        payload: Vec<u8>,
    }
    fn header(bytes: &[u8], at: &mut usize) -> (usize, usize, (usize, usize, bool)) {
        let read = |at: &mut usize| {
            let value = u32::from_be_bytes(bytes[*at..*at + 4].try_into().unwrap()) as usize;
            *at += 4;
            value
        };
        let cols = read(at);
        let rows = read(at);
        let col = read(at);
        let row = read(at);
        let visible = bytes[*at] == 1;
        *at += 1;
        (cols, rows, (col, row, visible))
    }
    fn read_view(bytes: &[u8], at: &mut usize) -> Readout {
        let (cols, rows, cursor) = header(bytes, at);
        let len = u32::from_be_bytes(bytes[*at..*at + 4].try_into().unwrap()) as usize;
        *at += 4;
        let payload = bytes[*at..*at + len].to_vec();
        *at += len;
        let mut offset = 0;
        let mut cells = 0;
        while offset < payload.len() {
            offset += cell_len(&payload[offset..]);
            cells += 1;
        }
        assert_eq!(cells, cols * rows, "view must carry every visible cell");
        Readout {
            cols,
            rows,
            cursor,
            rows_changed: (0..rows).collect(),
            payload,
        }
    }
    fn read_changed(bytes: &[u8], at: &mut usize) -> Readout {
        let (cols, rows, cursor) = header(bytes, at);
        let count = u32::from_be_bytes(bytes[*at..*at + 4].try_into().unwrap()) as usize;
        *at += 4;
        let mut rows_changed = Vec::new();
        let mut payload = Vec::new();
        for _ in 0..count {
            rows_changed.push(u32::from_be_bytes(bytes[*at..*at + 4].try_into().unwrap()) as usize);
            *at += 4;
            for _ in 0..cols {
                let len = cell_len(&bytes[*at..]);
                payload.extend_from_slice(&bytes[*at..*at + len]);
                *at += len;
            }
        }
        Readout {
            cols,
            rows,
            cursor,
            rows_changed,
            payload,
        }
    }

    #[test]
    fn view_encodes_cells() {
        let mut vt = avt::Vt::builder().size(4, 2).scrollback_limit(0).build();
        vt.feed_str("\x1b[1;3;4;5;7;9m\x1b[38;5;200m\x1b[48;2;1;2;3m界");
        let mut out = Vec::new();
        view(&vt, &mut out, MAX_VIEW).unwrap();
        let mut at = 0;
        let readout = read_view(&out, &mut at);
        assert_eq!(at, out.len());
        assert_eq!((readout.cols, readout.rows), (4, 2));
        assert_eq!(readout.cursor, (2, 0, true));
        // Wide head: codepoint, width 2, indexed foreground, RGB background,
        // bold|italic|underline|strikethrough|blink|inverse.
        assert_eq!(readout.payload[0..4], ('界' as u32).to_be_bytes());
        assert_eq!(readout.payload[4], 2);
        assert_eq!(readout.payload[5..7], [1, 200]);
        assert_eq!(readout.payload[7..11], [2, 1, 2, 3]);
        assert_eq!(readout.payload[11], 0x7d);
        // Wide tail: width 0, same pen.
        assert_eq!(readout.payload[12..16], (' ' as u32).to_be_bytes());
        assert_eq!(readout.payload[16], 0);
        // Untouched cell: default pen, single width.
        assert_eq!(readout.payload[24..28], (' ' as u32).to_be_bytes());
        assert_eq!(readout.payload[28..32], [1, 0, 0, 0]);
    }
    #[test]
    fn faint_and_default_colours_encode() {
        let mut vt = avt::Vt::builder().size(2, 1).scrollback_limit(0).build();
        vt.feed_str("\x1b[2mx");
        let mut out = Vec::new();
        view(&vt, &mut out, MAX_VIEW).unwrap();
        let mut at = 0;
        let readout = read_view(&out, &mut at);
        assert_eq!(readout.payload[4..8], [1, 0, 0, 0x02]);
    }
    #[test]
    fn changed_rows_accumulate_and_clear() {
        let mut vt = avt::Vt::builder().size(4, 3).scrollback_limit(0).build();
        let mut dirty = vec![true; 3];
        vt.feed_str("");
        let mut out = Vec::new();
        changed(&vt, &dirty, &mut out, MAX_VIEW).unwrap();
        let mut at = 0;
        assert_eq!(read_changed(&out, &mut at).rows_changed, vec![0, 1, 2]);
        assert_eq!(at, out.len());
        dirty.fill(false);
        out.clear();
        changed(&vt, &dirty, &mut out, MAX_VIEW).unwrap();
        let mut at = 0;
        assert_eq!(read_changed(&out, &mut at).rows_changed, Vec::<usize>::new());
        assert_eq!(at, out.len());
        // A pen change without printing touches no row; the print that follows does.
        for line in vt.feed_str("\x1b[31m\x1b7\x1b[4h\x1b[4l").lines {
            dirty[line] = true;
        }
        out.clear();
        changed(&vt, &dirty, &mut out, MAX_VIEW).unwrap();
        assert_eq!(read_changed(&out, &mut 0).rows_changed, Vec::<usize>::new());
        for line in vt.feed_str("X").lines {
            dirty[line] = true;
        }
        out.clear();
        changed(&vt, &dirty, &mut out, MAX_VIEW).unwrap();
        assert_eq!(read_changed(&out, &mut 0).rows_changed, vec![0]);
    }
    #[test]
    fn view_bound_rejects_oversized_readout() {
        let vt = avt::Vt::builder().size(80, 24).scrollback_limit(0).build();
        assert!(view(&vt, &mut Vec::new(), 16).is_err());
        let dirty = vec![true; 24];
        assert!(changed(&vt, &dirty, &mut Vec::new(), 16).is_err());
        assert!(view(&vt, &mut Vec::new(), MAX_VIEW).is_ok());
        // A failed bound writes nothing at all, so the stream stays framed.
        let mut out = Vec::new();
        assert!(view(&vt, &mut out, 16).is_err());
        assert!(out.is_empty());
    }
    #[test]
    fn readout_protocol_interleaves_with_dump() {
        let mut input = b"AVT1".to_vec();
        input.extend_from_slice(&4u32.to_be_bytes());
        input.extend_from_slice(&2u32.to_be_bytes());
        input.push(b'W');
        input.push(b'W');
        input.push(b'F');
        input.extend_from_slice(&2u32.to_be_bytes());
        input.extend_from_slice(b"ab");
        input.push(b'W');
        input.push(b'V');
        input.push(b'W');
        input.push(b'R');
        input.extend_from_slice(&6u32.to_be_bytes());
        input.extend_from_slice(&3u32.to_be_bytes());
        input.push(b'W');
        input.push(b'D');
        input.push(b'Q');
        let mut out = Vec::new();
        run(&mut &input[..], &mut out).unwrap();
        let mut at = 0;
        assert_eq!(read_changed(&out, &mut at).rows_changed, vec![0, 1]);
        assert_eq!(read_changed(&out, &mut at).rows_changed, Vec::<usize>::new());
        let printed = read_changed(&out, &mut at);
        assert_eq!(printed.rows_changed, vec![0]);
        assert_eq!(printed.cursor, (2, 0, true));
        let full = read_view(&out, &mut at);
        assert_eq!((full.cols, full.rows), (4, 2));
        assert_eq!(read_changed(&out, &mut at).rows_changed, Vec::<usize>::new());
        let resized = read_changed(&out, &mut at);
        assert_eq!((resized.cols, resized.rows), (6, 3));
        assert_eq!(resized.rows_changed, vec![0, 1, 2]);
        // The dump reply still follows the readouts on the same stream.
        let cols = u32::from_be_bytes(out[at..at + 4].try_into().unwrap()) as usize;
        let rows = u32::from_be_bytes(out[at + 4..at + 8].try_into().unwrap()) as usize;
        let len = u32::from_be_bytes(out[at + 8..at + 12].try_into().unwrap()) as usize;
        assert_eq!((cols, rows), (6, 3));
        assert_eq!(at + 12 + len, out.len());
        assert!(std::str::from_utf8(&out[at + 12..]).unwrap().contains("ab"));
    }
    #[test]
    fn unknown_readout_like_operation_is_rejected() {
        let mut input = b"AVT1".to_vec();
        input.extend_from_slice(&4u32.to_be_bytes());
        input.extend_from_slice(&2u32.to_be_bytes());
        input.push(b'v');
        assert!(run(&mut &input[..], &mut Vec::new()).is_err());
    }
    #[test]
    fn protocol_handshake_reports_version() {
        let mut input = b"AVT1".to_vec();
        input.extend_from_slice(&4u32.to_be_bytes());
        input.extend_from_slice(&2u32.to_be_bytes());
        input.push(b'P');
        input.push(b'D');
        input.push(b'Q');
        let mut out = Vec::new();
        run(&mut &input[..], &mut out).unwrap();
        assert_eq!(&out[0..4], b"AVTP");
        assert_eq!(u32::from_be_bytes(out[4..8].try_into().unwrap()), 2);
        assert_eq!(PROTOCOL_VERSION, 2);
        // The session continues normally after the handshake.
        assert_eq!(u32::from_be_bytes(out[8..12].try_into().unwrap()), 4);
        assert_eq!(u32::from_be_bytes(out[12..16].try_into().unwrap()), 2);
        let len = u32::from_be_bytes(out[16..20].try_into().unwrap()) as usize;
        assert_eq!(20 + len, out.len());
    }
}
