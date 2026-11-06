// SPDX-License-Identifier: GPL-2.0-only

package item

import (
	"math"
	"strconv"
	"strings"
	"unicode"
	"unicode/utf8"
)

// DefaultSortOptions returns fresh upstream default sort-token slices.
func DefaultSortOptions() SortOptions {
	return SortOptions{
		RemoveWords:       []string{"the", "a", "an"},
		RemoveCharacters:  []string{",", "&", "-", "{", "}", "'"},
		ReplaceCharacters: []string{".", "+", "%"},
	}
}

// ComputeSortName returns the computed sort name and whether it is present.
func ComputeSortName(it Item, opts SortOptions) (string, bool) {
	if it.SortName != nil {
		return *it.SortName, true
	}
	if it.ForcedSortName != nil && *it.ForcedSortName != "" {
		return sortName(*it.ForcedSortName, it.Kind != KindPerson, opts)
	}

	switch it.Kind {
	case KindAudio, KindAudioBook:
		return indexedSortName(it, 4), true
	case KindEpisode:
		return episodeSortName(it), true
	case KindSeason:
		if it.IndexNumber != nil {
			return formatPadded(*it.IndexNumber, 4), true
		}
		if it.Name == nil {
			return "", false
		}
		return *it.Name, true
	case KindLiveTvChannel, KindTvChannel:
		return liveTVSortName(it), true
	default:
		return sortNamePointer(it.Name, it.Kind != KindPerson, opts)
	}
}

func sortNamePointer(value *string, alphaEnabled bool, opts SortOptions) (string, bool) {
	if value == nil {
		return "", false
	}
	return sortName(*value, alphaEnabled, opts)
}

func sortName(value string, alphaEnabled bool, opts SortOptions) (string, bool) {
	if !alphaEnabled {
		return strings.TrimLeftFunc(value, unicode.IsSpace), true
	}

	value = invariantLower(strings.TrimSpace(value))
	opts = resolvedSortOptions(opts)
	for _, word := range opts.RemoveWords {
		if strings.HasPrefix(value, word+" ") {
			value = strings.TrimPrefix(value, word+" ")
		}
		value = strings.ReplaceAll(value, " "+word+" ", " ")
		if strings.HasSuffix(value, " "+word) {
			value = strings.TrimSuffix(value, " "+word)
		}
	}
	for _, character := range opts.RemoveCharacters {
		value = strings.ReplaceAll(value, character, "")
	}
	for _, character := range opts.ReplaceCharacters {
		value = strings.ReplaceAll(value, character, " ")
	}

	value = padSortDigitRuns(value)
	value = removeDiacritics(value)
	if containsNonASCII(value) && opts.TransliterateNonASCII != nil {
		value = opts.TransliterateNonASCII(value)
	}
	return value, true
}

func resolvedSortOptions(opts SortOptions) SortOptions {
	defaults := DefaultSortOptions()
	if opts.RemoveWords == nil {
		opts.RemoveWords = defaults.RemoveWords
	}
	if opts.RemoveCharacters == nil {
		opts.RemoveCharacters = defaults.RemoveCharacters
	}
	if opts.ReplaceCharacters == nil {
		opts.ReplaceCharacters = defaults.ReplaceCharacters
	}
	return opts
}

func indexedSortName(it Item, width int) string {
	var out strings.Builder
	if it.ParentIndexNumber != nil {
		out.WriteString(formatPadded(*it.ParentIndexNumber, width))
		out.WriteString(" - ")
	}
	if it.IndexNumber != nil {
		out.WriteString(formatPadded(*it.IndexNumber, width))
		out.WriteString(" - ")
	}
	out.WriteString(itemName(it))
	return out.String()
}

func episodeSortName(it Item) string {
	var out strings.Builder
	if it.ParentIndexNumber != nil {
		out.WriteString(formatPadded(*it.ParentIndexNumber, 3))
		out.WriteString(" - ")
	}
	if it.IndexNumber != nil {
		out.WriteString(formatPadded(*it.IndexNumber, 4))
		out.WriteString(" - ")
	}
	out.WriteString(itemName(it))
	return out.String()
}

func liveTVSortName(it Item) string {
	numberText := ""
	if it.Extra.LiveTV != nil && it.Extra.LiveTV.Number != nil {
		numberText = *it.Extra.LiveTV.Number
	}
	if number, ok := parseInvariantDouble(numberText); ok {
		return formatLiveTVNumber(number) + "-" + itemName(it)
	}
	return numberText + "-" + itemName(it)
}

// trimInvariantNumberSpace accepts exactly the whitespace consumed by .NET's
// invariant numeric parser: tab through carriage return and ordinary space.
func trimInvariantNumberSpace(value string) string {
	start := 0
	for start < len(value) && isInvariantNumberSpace(value[start]) {
		start++
	}
	end := len(value)
	for end > start && isInvariantNumberSpace(value[end-1]) {
		end--
	}
	return value[start:end]
}

func isInvariantNumberSpace(value byte) bool {
	return value == ' ' || (value >= '\t' && value <= '\r')
}

func parseInvariantDouble(value string) (float64, bool) {
	value = trimInvariantNumberSpace(value)
	if value == "" {
		return 0, false
	}

	switch {
	case strings.EqualFold(value, "Infinity"), strings.EqualFold(value, "+Infinity"):
		return math.Inf(1), true
	case strings.EqualFold(value, "-Infinity"):
		return math.Inf(-1), true
	case strings.EqualFold(value, "NaN"):
		return math.NaN(), true
	}

	decimal, ok := invariantDecimal(value)
	if !ok {
		return 0, false
	}
	number, err := strconv.ParseFloat(decimal, 64)
	if err != nil && !math.IsInf(number, 0) {
		return 0, false
	}
	return number, true
}

// invariantDecimal accepts the decimal grammar used by .NET invariant double
// parsing and compacts validated integral group separators for strconv.
func invariantDecimal(value string) (string, bool) {
	index := 0
	if value[index] == '+' || value[index] == '-' {
		index++
	}

	commaCount := 0
	integerDigits := 0
	for index < len(value) && (value[index] >= '0' && value[index] <= '9' || value[index] == ',') {
		if value[index] == ',' {
			if integerDigits == 0 {
				return "", false
			}
			commaCount++
		} else {
			integerDigits++
		}
		index++
	}

	fractionDigits := 0
	if index < len(value) && value[index] == '.' {
		index++
		fractionStart := index
		for index < len(value) && value[index] >= '0' && value[index] <= '9' {
			index++
		}
		fractionDigits = index - fractionStart
	}
	if integerDigits == 0 && fractionDigits == 0 {
		return "", false
	}

	if index < len(value) && (value[index] == 'e' || value[index] == 'E') {
		index++
		if index < len(value) && (value[index] == '+' || value[index] == '-') {
			index++
		}
		exponentStart := index
		for index < len(value) && value[index] >= '0' && value[index] <= '9' {
			index++
		}
		if index == exponentStart {
			return "", false
		}
	}
	if index != len(value) {
		return "", false
	}
	if commaCount == 0 {
		return value, true
	}

	var decimal strings.Builder
	decimal.Grow(len(value) - commaCount)
	for index := range value {
		if value[index] != ',' {
			decimal.WriteByte(value[index])
		}
	}
	return decimal.String(), true
}

func formatLiveTVNumber(number float64) string {
	switch {
	case math.IsNaN(number):
		return "NaN"
	case math.IsInf(number, 1):
		return "Infinity"
	case math.IsInf(number, -1):
		return "-Infinity"
	}

	formatted := strconv.FormatFloat(number, 'f', 5, 64)
	negative := strings.HasPrefix(formatted, "-")
	if negative {
		formatted = formatted[1:]
	}
	if formatted == "0.00000" {
		negative = false
	}
	integer, fraction, found := strings.Cut(formatted, ".")
	if !found {
		return strconv.FormatFloat(number, 'f', 5, 64)
	}
	if len(integer) < 10 {
		integer = strings.Repeat("0", 10-len(integer)) + integer
	}
	if negative {
		return "-" + integer + "." + fraction
	}
	return integer + "." + fraction
}

func padSortDigitRuns(value string) string {
	var out strings.Builder
	out.Grow(len(value))
	for start := 0; start < len(value); {
		r, size := utf8RuneAt(value, start)
		if !isSortDigit(r) {
			out.WriteString(value[start : start+size])
			start += size
			continue
		}

		end := start + size
		count := 1
		for end < len(value) {
			next, nextSize := utf8RuneAt(value, end)
			if !isSortDigit(next) {
				break
			}
			count++
			end += nextSize
		}
		if count < 10 {
			out.WriteString(strings.Repeat("0", 10-count))
		}
		out.WriteString(value[start:end])
		start = end
	}
	return out.String()
}

func utf8RuneAt(value string, index int) (rune, int) {
	r, size := rune(value[index]), 1
	if r >= 0x80 {
		r, size = utf8.DecodeRuneInString(value[index:])
	}
	return r, size
}

func isSortDigit(r rune) bool {
	return r <= 0xFFFF && unicode.IsDigit(r)
}

func containsNonASCII(value string) bool {
	for i := 0; i < len(value); i++ {
		if value[i] >= 0x80 {
			return true
		}
	}
	return false
}

// ParseInvariantDouble reports whether value parses as .NET
// double.TryParse(value, CultureInfo.InvariantCulture) does, and returns the
// number. Live TV channel numbers use it (M3uParser.cs:170-265).
func ParseInvariantDouble(value string) (float64, bool) {
	return parseInvariantDouble(value)
}
