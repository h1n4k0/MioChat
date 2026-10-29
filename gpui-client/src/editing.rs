use std::ops::Range;
use unicode_segmentation::UnicodeSegmentation;

pub fn byte_offset(text: &str, utf16: usize) -> usize {
    let mut count = 0;
    for (byte, ch) in text.char_indices() {
        if count + ch.len_utf16() > utf16 {
            return byte;
        }
        count += ch.len_utf16();
    }
    text.len()
}
pub fn utf16_offset(text: &str, byte: usize) -> usize {
    let mut end = byte.min(text.len());
    while !text.is_char_boundary(end) {
        end -= 1;
    }
    text[..end].encode_utf16().count()
}
pub fn byte_range(text: &str, range: Range<usize>) -> Range<usize> {
    let start = byte_offset(text, range.start);
    start..byte_offset(text, range.end.max(range.start))
}
pub fn utf16_range(text: &str, range: Range<usize>) -> Range<usize> {
    utf16_offset(text, range.start)..utf16_offset(text, range.end)
}
pub fn replace(
    text: &mut String,
    selection: &mut Range<usize>,
    marked: &mut Option<Range<usize>>,
    range_utf16: Option<Range<usize>>,
    insert: &str,
    max_chars: usize,
) -> bool {
    let range = range_utf16
        .map(|r| byte_range(text, r))
        .or_else(|| marked.clone())
        .unwrap_or_else(|| selection.clone());
    if range.start > range.end
        || range.end > text.len()
        || !text.is_char_boundary(range.start)
        || !text.is_char_boundary(range.end)
    {
        return false;
    }
    let new_count =
        text.chars().count() - text[range.clone()].chars().count() + insert.chars().count();
    if new_count > max_chars {
        return false;
    }
    let end = range.start + insert.len();
    text.replace_range(range, insert);
    *selection = end..end;
    *marked = None;
    true
}
fn floor_boundary(text: &str, offset: usize) -> usize {
    let mut offset = offset.min(text.len());
    while !text.is_char_boundary(offset) {
        offset -= 1;
    }
    offset
}
pub fn valid_selection(text: &str, selection: Range<usize>) -> Range<usize> {
    let start = floor_boundary(text, selection.start);
    start..floor_boundary(text, selection.end).max(start)
}
pub fn previous(text: &str, offset: usize) -> usize {
    text[..floor_boundary(text, offset)]
        .grapheme_indices(true)
        .next_back()
        .map_or(0, |(i, _)| i)
}
pub fn next(text: &str, offset: usize) -> usize {
    let offset = floor_boundary(text, offset);
    text[offset..]
        .graphemes(true)
        .next()
        .map_or(text.len(), |g| offset + g.len())
}
#[cfg(test)]
mod tests {
    use super::*;
    #[test]
    fn stale_cursor_inside_a_multibyte_character_is_safe() {
        let text = "中文😀draft";
        for offset in 0..text.len() + 3 {
            assert!(text.is_char_boundary(previous(text, offset)));
            assert!(text.is_char_boundary(next(text, offset)));
            let selection = valid_selection(text, offset..offset + 1);
            assert!(text.get(selection).is_some());
        }
    }
    #[test]
    fn utf16_surrogates_and_ime_replacement_preserve_text() {
        let mut value = "中文😀".to_string();
        let mut selection = value.len()..value.len();
        let mut marked = None;
        assert_eq!(byte_range(&value, 2..4), 6..10);
        assert!(replace(
            &mut value,
            &mut selection,
            &mut marked,
            Some(2..4),
            "界",
            1600
        ));
        assert_eq!(value, "中文界");
        assert!(replace(
            &mut value,
            &mut selection,
            &mut marked,
            None,
            "ni",
            1600
        ));
        marked = Some(9..11);
        assert!(replace(
            &mut value,
            &mut selection,
            &mut marked,
            None,
            "你",
            1600
        ));
        assert_eq!(value, "中文界你");
        assert_eq!(utf16_range(&value, selection), 4..4);
    }
    #[test]
    fn selection_paste_limits_and_grapheme_backspace() {
        let mut text = "a👨‍👩‍👧‍👦b".to_string();
        let end = text.len() - 1;
        assert_eq!(previous(&text, end), 1);
        let mut selection = 1..end;
        let mut marked = None;
        assert!(replace(
            &mut text,
            &mut selection,
            &mut marked,
            None,
            "中文",
            4
        ));
        assert_eq!(text, "a中文b");
        assert!(!replace(
            &mut text,
            &mut selection,
            &mut marked,
            None,
            "超长",
            4
        ));
    }
}
