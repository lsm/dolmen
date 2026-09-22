package postgres

import (
	"encoding/json"
	"fmt"
	"math"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/lsm/dolmen/internal/filter"
	"github.com/lsm/dolmen/internal/schema"
	"github.com/lsm/dolmen/internal/store"
)

const julianUnixEpoch = 2440587.5

var sqliteDatedTime = regexp.MustCompile(`^(\d{4})-(\d{2})-(\d{2})(?:[T ](\d{2}):(\d{2})(?::(\d{2})(?:\.(\d+))?)?([Zz]|[+-]\d{2}:\d{2})?)?$`)

var sqliteBareTime = regexp.MustCompile(`^(\d{2}):(\d{2})(?::(\d{2})(?:\.(\d+))?)?([Zz]|[+-]\d{2}:\d{2})?$`)

var sqliteOffsetModifier = regexp.MustCompile(`^([+-])(\d{2}):(\d{2})(?::(\d{2})(?:\.(\d+))?)?$`)

var sqliteAmountModifier = regexp.MustCompile(`^([+-]?(?:\d+\.?\d*|\.\d+)(?:[eE][+-]?\d+)?)[ \t]+([A-Za-z]+)$`)

var sqliteModifierUnits = map[string]float64{
	"day": 86400, "days": 86400,
	"hour": 3600, "hours": 3600,
	"minute": 60, "minutes": 60,
	"second": 1, "seconds": 1,
}

var sqliteWideningUnits = map[string]bool{
	"month": true, "months": true, "year": true, "years": true,
}

var sqliteNamedModifiers = map[string]bool{
	"start of day": true, "start of month": true, "start of year": true,
	"auto": true, "julianday": true, "unixepoch": true, "subsec": true,
	"subsecond": true, "localtime": true, "utc": true, "ceiling": true, "floor": true,
}

var sqliteWeekdayModifier = regexp.MustCompile(`^weekday \d+$`)

type timeTextKind int

const (
	timeReadable timeTextKind = iota
	timeClockDependent
	timeUnrendered
	timeMalformed
)

var sqliteClockWords = map[string]bool{"now": true, "localtime": true, "utc": true}

func julianToTime(jd float64) time.Time {
	seconds := (jd - julianUnixEpoch) * 86400
	whole := math.Floor(seconds)
	return time.Unix(int64(whole), int64(math.Round((seconds-whole)*1e9))).UTC()
}

func parseSQLiteJulian(text string) (time.Time, bool) {
	jd, err := strconv.ParseFloat(strings.TrimSpace(text), 64)
	if err != nil || jd < 0 || jd > 5373484.5 {
		return time.Time{}, false
	}
	return julianToTime(jd), true
}

func parseSQLiteClock(hour, minute, second, fraction, zone string) (time.Duration, timeTextKind) {
	h, _ := strconv.Atoi(hour)
	m, _ := strconv.Atoi(minute)
	s := 0
	if second != "" {
		s, _ = strconv.Atoi(second)
	}
	if h > 24 || m > 59 || s > 59 {
		return 0, timeMalformed
	}
	if h == 24 {
		return 0, timeUnrendered
	}
	d := time.Duration(h)*time.Hour + time.Duration(m)*time.Minute + time.Duration(s)*time.Second
	if fraction != "" {
		scaled := fraction
		for len(scaled) < 9 {
			scaled += "0"
		}
		nanos, err := strconv.Atoi(scaled[:9])
		if err != nil {
			return 0, timeMalformed
		}
		d += time.Duration(nanos)
	}
	if zone != "" && zone != "Z" && zone != "z" {
		sign := time.Duration(1)
		if zone[0] == '-' {
			sign = -1
		}
		oh, _ := strconv.Atoi(zone[1:3])
		om, _ := strconv.Atoi(zone[4:6])
		d -= sign * (time.Duration(oh)*time.Hour + time.Duration(om)*time.Minute)
	}
	return d, timeReadable
}

func parseSQLiteTimeText(text string) (time.Time, timeTextKind) {
	if moment, ok := parseSQLiteJulian(text); ok {
		return moment, timeReadable
	}
	trimmed := strings.TrimRight(text, " \t\n\r")
	if sqliteClockWords[strings.ToLower(strings.TrimSpace(trimmed))] {
		return time.Time{}, timeClockDependent
	}
	if m := sqliteDatedTime.FindStringSubmatch(trimmed); m != nil {
		year, _ := strconv.Atoi(m[1])
		month, _ := strconv.Atoi(m[2])
		day, _ := strconv.Atoi(m[3])
		if month < 1 || month > 12 || day < 1 {
			return time.Time{}, timeMalformed
		}
		clock := time.Duration(0)
		if m[4] != "" {
			var kind timeTextKind
			if clock, kind = parseSQLiteClock(m[4], m[5], m[6], m[7], m[8]); kind != timeReadable {
				return time.Time{}, kind
			}
		}
		return time.Date(year, time.January, 1, 0, 0, 0, 0, time.UTC).
			AddDate(0, month-1, day-1).Add(clock), timeReadable
	}
	if m := sqliteBareTime.FindStringSubmatch(trimmed); m != nil {
		clock, kind := parseSQLiteClock(m[1], m[2], m[3], m[4], m[5])
		if kind != timeReadable {
			return time.Time{}, kind
		}
		return time.Date(2000, time.January, 1, 0, 0, 0, 0, time.UTC).Add(clock), timeReadable
	}
	return time.Time{}, timeMalformed
}

type filterSupport int

const (
	supportedHere filterSupport = iota
	sqliteOnly
	malformedInSQLite
)

func parseSQLiteModifier(text string) (seconds float64, support filterSupport) {
	lowered := strings.ToLower(text)
	if sqliteNamedModifiers[lowered] || sqliteWeekdayModifier.MatchString(lowered) {
		return 0, sqliteOnly
	}
	if m := sqliteOffsetModifier.FindStringSubmatch(text); m != nil {
		hours, _ := strconv.Atoi(m[2])
		minutes, _ := strconv.Atoi(m[3])
		total := float64(hours)*3600 + float64(minutes)*60
		if m[4] != "" {
			extra, _ := strconv.Atoi(m[4])
			total += float64(extra)
		}
		if m[5] != "" {
			frac, err := strconv.ParseFloat("0."+m[5], 64)
			if err != nil {
				return 0, malformedInSQLite
			}
			total += frac
		}
		if m[1] == "-" {
			total = -total
		}
		return total, supportedHere
	}
	if m := sqliteAmountModifier.FindStringSubmatch(text); m != nil {
		unit := strings.ToLower(m[2])
		amount, err := strconv.ParseFloat(m[1], 64)
		if err != nil {
			return 0, malformedInSQLite
		}
		if scale, ok := sqliteModifierUnits[unit]; ok {
			return amount * scale, supportedHere
		}
		if sqliteWideningUnits[unit] {
			return 0, sqliteOnly
		}
	}
	return 0, malformedInSQLite
}

var strftimePatterns = map[byte]string{
	'Y': "YYYY", 'm': "MM", 'd': "DD", 'H': "HH24", 'M': "MI", 'S': "SS", 'j': "DDD",
}

var strftimeKnownCodes = "deffFGgHIjJkKlmMpPRsSTuUVwWYy%"

func strftimeToCharPattern(format string) (pattern string, unsupported byte, support filterSupport) {
	var out strings.Builder
	var literal strings.Builder
	flush := func() {
		if literal.Len() == 0 {
			return
		}
		out.WriteString(`"` + strings.ReplaceAll(literal.String(), `"`, `\"`) + `"`)
		literal.Reset()
	}
	for i := 0; i < len(format); i++ {
		if format[i] != '%' {
			literal.WriteByte(format[i])
			continue
		}
		if i+1 >= len(format) {
			return "", 0, malformedInSQLite
		}
		i++
		code := format[i]
		if code == '%' {
			literal.WriteByte('%')
			continue
		}
		if mapped, ok := strftimePatterns[code]; ok {
			flush()
			out.WriteString(mapped)
			continue
		}
		if strings.IndexByte(strftimeKnownCodes, code) >= 0 {
			return "", code, sqliteOnly
		}
		return "", 0, malformedInSQLite
	}
	flush()
	return out.String(), 0, supportedHere
}

var timeOutputPatterns = map[string]string{
	"date":     "YYYY-MM-DD",
	"time":     "HH24:MI:SS",
	"datetime": "YYYY-MM-DD HH24:MI:SS",
}

const timestampTextShape = `^[0-9]{4}-[0-9]{2}-[0-9]{2}([T ][0-9]{2}:[0-9]{2}(:[0-9]{2}(\.[0-9]+)?)?([Zz]|[+-][0-9]{2}:[0-9]{2})?)?$`

const timestampTextZone = `([Zz]|[+-][0-9]{2}:[0-9]{2})$`

func timestampLiteral(moment time.Time) string {
	return "TIMESTAMP " + dollarQuote(moment.UTC().Format("2006-01-02 15:04:05.999999"))
}

func timestampColumn(expr string) string {
	return "(CASE WHEN " + expr + " !~ " + dollarQuote(timestampTextShape) + " THEN NULL" +
		" WHEN " + expr + " ~ " + dollarQuote(timestampTextZone) +
		" THEN (CASE WHEN pg_catalog.pg_input_is_valid(" + expr + ", 'timestamptz')" +
		" THEN " + expr + "::timestamptz AT TIME ZONE 'UTC' END)" +
		" ELSE (CASE WHEN pg_catalog.pg_input_is_valid(" + expr + ", 'timestamp')" +
		" THEN " + expr + "::timestamp END) END)"
}

func timeTextMoment(text string) (string, error) {
	moment, kind := parseSQLiteTimeText(text)
	switch kind {
	case timeReadable:
		return timestampLiteral(moment), nil
	case timeMalformed:
		return "NULL::timestamp", nil
	case timeClockDependent:
		return "", fmt.Errorf("%w: %q reads the server's clock, so the same filter would mean different things at different moments; compute the moment you mean and bind it as a ? argument", store.ErrInvalid, text)
	}
	return "", filterNotRenderable("the time " + strconv.Quote(text))
}

func julianMoment(value float64) (string, error) {
	if value < 0 || value > 5373484.5 {
		return "NULL::timestamp", nil
	}
	return timestampLiteral(julianToTime(value)), nil
}

func (r *filterRenderer) constantText(n filter.Node) (string, bool) {
	switch node := n.(type) {
	case *filter.Literal:
		if node.Kind == filter.LiteralString {
			return node.Text, true
		}
	case *filter.Param:
		if node.Index < len(r.args) {
			if text, ok := r.args[node.Index].(string); ok {
				return text, true
			}
		}
	}
	return "", false
}

func (r *filterRenderer) literalMoment(node *filter.Literal) (string, error) {
	switch node.Kind {
	case filter.LiteralNull:
		return "NULL::timestamp", nil
	case filter.LiteralString:
		return timeTextMoment(node.Text)
	case filter.LiteralNumber:
		lowered := strings.ToLower(node.Text)
		if strings.HasPrefix(lowered, "0x") {
			value, err := strconv.ParseUint(lowered[2:], 16, 64)
			if err != nil {
				return "", fmt.Errorf("%w: the hexadecimal literal %q does not fit in 64 bits", store.ErrInvalid, node.Text)
			}
			return julianMoment(float64(int64(value)))
		}
		value, err := strconv.ParseFloat(node.Text, 64)
		if err != nil {
			return "NULL::timestamp", nil
		}
		return julianMoment(value)
	}
	return "", filterNotRenderable("a date or time function over " + node.Text)
}

func (r *filterRenderer) paramMoment(node *filter.Param) (string, error) {
	if node.Index >= len(r.args) {
		return "", fmt.Errorf("%w: filter argument %d was not supplied", store.ErrInvalid, node.Index+1)
	}
	switch value := r.args[node.Index].(type) {
	case nil:
		return "NULL::timestamp", nil
	case string:
		return timeTextMoment(value)
	case float64:
		return julianMoment(value)
	case int64:
		return julianMoment(float64(value))
	case int:
		return julianMoment(float64(value))
	case json.Number:
		parsed, err := value.Float64()
		if err != nil {
			return "NULL::timestamp", nil
		}
		return julianMoment(parsed)
	}
	return "", filterNotRenderable(fmt.Sprintf("a date or time function over a %T argument", r.args[node.Index]))
}

func (r *filterRenderer) columnMoment(node *filter.Column) (string, error) {
	if r.types[node.Name] != schema.Timestamp {
		return "", filterNotRenderable("a date or time function over the column " + strconv.Quote(node.Name) + ", which is not a timestamp field")
	}
	physical, ok := r.columns[node.Name]
	if !ok {
		physical = node.Name
	}
	return timestampColumn(ident(physical)), nil
}

func (r *filterRenderer) nestedMoment(node *filter.Call, next int) (string, error) {
	inner, err := r.moment(node, next)
	if err != nil {
		return "", err
	}
	switch node.Name {
	case "date":
		return "pg_catalog.date_trunc('day', " + inner + ")", nil
	case "time":
		return "(TIMESTAMP '2000-01-01' + (pg_catalog.date_trunc('second', " + inner + ") - pg_catalog.date_trunc('day', " + inner + ")))", nil
	case "datetime":
		return "pg_catalog.date_trunc('second', " + inner + ")", nil
	case "julianday":
		return inner, nil
	}
	return "", filterNotRenderable("a date or time function over the text " + node.Name + " returns")
}

func (r *filterRenderer) moment(node *filter.Call, next int) (string, error) {
	args := node.Args
	if node.Name == "strftime" {
		args = args[1:]
	}
	if len(args) == 0 {
		return "", filterNotRenderable(node.Name + " without a time")
	}
	base, err := r.baseMoment(args[0], next)
	if err != nil {
		return "", err
	}
	shift := 0.0
	for _, arg := range args[1:] {
		text, ok := r.constantText(arg)
		if !ok {
			return "", filterNotRenderable("a date or time modifier that is not text")
		}
		seconds, support := parseSQLiteModifier(text)
		switch support {
		case sqliteOnly:
			return "", filterNotRenderable("the modifier " + strconv.Quote(text))
		case malformedInSQLite:
			return "NULL::timestamp", nil
		}
		shift += seconds
	}
	if shift == 0 {
		return base, nil
	}
	return "(" + base + " + pg_catalog.make_interval(secs => " + strconv.FormatFloat(shift, 'f', -1, 64) + "))", nil
}

func (r *filterRenderer) baseMoment(n filter.Node, next int) (string, error) {
	switch node := n.(type) {
	case *filter.Literal:
		return r.literalMoment(node)
	case *filter.Param:
		return r.paramMoment(node)
	case *filter.Column:
		return r.columnMoment(node)
	case *filter.Call:
		return r.nestedMoment(node, next)
	}
	return "", filterNotRenderable("a date or time function over this expression")
}

func (r *filterRenderer) timeCall(node *filter.Call, next int) error {
	pattern, ok := timeOutputPatterns[node.Name]
	if node.Name == "strftime" {
		format, isText := r.constantText(node.Args[0])
		if !isText {
			return filterNotRenderable("a strftime format that is not text")
		}
		rendered, unsupported, support := strftimeToCharPattern(format)
		switch support {
		case sqliteOnly:
			return filterNotRenderable(fmt.Sprintf("the strftime field %%%c", unsupported))
		case malformedInSQLite:
			r.sb.WriteString("NULL::text")
			return nil
		}
		pattern, ok = rendered, true
	}
	moment, err := r.moment(node, next)
	if err != nil {
		return err
	}
	if !ok {
		r.sb.WriteString("(pg_catalog.date_part('epoch', " + moment + ") / 86400.0 + " +
			strconv.FormatFloat(julianUnixEpoch, 'f', -1, 64) + ")")
		return nil
	}
	r.sb.WriteString("pg_catalog.to_char(" + moment + ", " + dollarQuote(pattern) + ")")
	return nil
}
