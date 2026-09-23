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

const julianUnixEpochMillis = julianUnixEpoch * 86400000

var sqliteZone = `([Zz]|[+-](?:0\d|1[0-4]):[0-5]\d)`

var sqliteDatedTime = regexp.MustCompile(`^(-?)(\d{4})-(\d{2})-(\d{2})(?:[T \t\n\v\f\r]*(\d{2}):(\d{2})(?::(\d{2})(?:\.(\d+))?)?` + sqliteZone + `?)?$`)

var sqliteBareTime = regexp.MustCompile(`^(\d{2}):(\d{2})(?::(\d{2})(?:\.(\d+))?)?` + sqliteZone + `?$`)

var sqliteOffsetModifier = regexp.MustCompile(`^([+-])(\d{2}):(\d{2})(?::(\d{2})(?:\.(\d+))?)?$`)

var sqliteAmountModifier = regexp.MustCompile(`^([+-]?(?:\d+\.?\d*|\.\d+)(?:[eE][+-]?\d+)?)[ \t\n\v\f\r]+([A-Za-z]+)$`)

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

func quantizeToMillis(seconds float64) int {
	millis := int(math.Round(seconds * 1000))
	if millis > 999 {
		return 999
	}
	return millis
}

func julianToTime(jd float64) time.Time {
	return time.UnixMilli(int64(float64(jd*86400000)+0.5) - julianUnixEpochMillis).UTC()
}

var sqliteRealNumber = regexp.MustCompile(`^[+-]?(?:\d+\.?\d*|\.\d+)(?:[eE][+-]?\d+)?$`)

const maxShiftMillis = 1e15

var firstRenderableMoment = time.Date(1, time.January, 1, 0, 0, 0, 0, time.UTC)

var lastRenderableMoment = time.Date(9999, time.December, 31, 23, 59, 59, 999000000, time.UTC)

func shiftedBy(t time.Time, millis int64) time.Time {
	return t.AddDate(0, 0, int(millis/86400000)).Add(time.Duration(millis%86400000) * time.Millisecond)
}

func parseSQLiteJulian(text string) (time.Time, bool) {
	trimmed := strings.TrimSpace(text)
	if !sqliteRealNumber.MatchString(trimmed) {
		return time.Time{}, false
	}
	jd, err := strconv.ParseFloat(trimmed, 64)
	if err != nil || math.IsNaN(jd) || jd < 0 || jd >= 5373484.5 {
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
		seconds, err := strconv.ParseFloat("0."+fraction, 64)
		if err != nil {
			return 0, timeMalformed
		}
		d += time.Duration(quantizeToMillis(seconds)) * time.Millisecond
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
		if moment.Year() < 1 {
			return time.Time{}, timeUnrendered
		}
		return moment, timeReadable
	}
	trimmed := strings.TrimRight(text, " \t\n\v\f\r")
	if sqliteClockWords[strings.ToLower(strings.TrimSpace(trimmed))] {
		return time.Time{}, timeClockDependent
	}
	if m := sqliteDatedTime.FindStringSubmatch(trimmed); m != nil {
		year, _ := strconv.Atoi(m[2])
		month, _ := strconv.Atoi(m[3])
		day, _ := strconv.Atoi(m[4])
		if month < 1 || month > 12 || day < 1 || day > 31 {
			return time.Time{}, timeMalformed
		}
		clock := time.Duration(0)
		if m[5] != "" {
			var kind timeTextKind
			if clock, kind = parseSQLiteClock(m[5], m[6], m[7], m[8], m[9]); kind != timeReadable {
				return time.Time{}, kind
			}
		}
		if m[1] == "-" {
			return time.Time{}, timeUnrendered
		}
		moment := time.Date(year, time.January, 1, 0, 0, 0, 0, time.UTC).
			AddDate(0, month-1, day-1).Add(clock)
		if moment.Year() < 1 {
			return time.Time{}, timeUnrendered
		}
		return moment, timeReadable
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
		seconds := 0
		if m[4] != "" {
			seconds, _ = strconv.Atoi(m[4])
		}
		if hours > 24 || minutes > 59 || seconds > 59 {
			return 0, malformedInSQLite
		}
		if hours == 24 {
			return 0, sqliteOnly
		}
		total := float64(hours)*3600 + float64(minutes)*60 + float64(seconds)
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
		escaped := strings.ReplaceAll(literal.String(), `\`, `\\`)
		out.WriteString(`"` + strings.ReplaceAll(escaped, `"`, `\"`) + `"`)
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

const timestampTextShape = `^[0-9]{4}-[0-9]{2}-[0-9]{2}([T ][0-9]{2}:[0-9]{2}(:[0-9]{2}(\.[0-9]+)?)?([Zz]|[+-](0[0-9]|1[0-4]):[0-5][0-9])?)?$`

const timestampTextZone = `([Zz]|[+-][0-9]{2}:[0-9]{2})$`

func timestampLiteral(moment time.Time) string {
	return "TIMESTAMP " + dollarQuote(moment.UTC().Format("2006-01-02 15:04:05.999999"))
}

func quantizedToSQLitesMillisecond(expr string) string {
	return "LEAST(pg_catalog.date_trunc('milliseconds', " + expr + " + INTERVAL '0.0005 second')," +
		" pg_catalog.date_trunc('second', " + expr + ") + INTERVAL '0.999 second')"
}

func withinRepresentableYears(expr string) string {
	return "(CASE WHEN " + expr + " BETWEEN TIMESTAMP '0001-01-01 00:00:00' AND TIMESTAMP '9999-12-31 23:59:59.999'" +
		" THEN " + expr + " END)"
}

func timestampColumn(raw string) string {
	expr := "pg_catalog.regexp_replace(" + raw + ", " + dollarQuote(`(\.[0-9]{4})[0-9]+`) + ", " + dollarQuote(`\1`) + ")"
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
	if math.IsNaN(value) || value < 0 || value >= 5373484.5 {
		return "NULL::timestamp", nil
	}
	moment := julianToTime(value)
	if moment.Year() < 1 {
		return "", filterNotRenderable("a moment before 0001-01-01, which PostgreSQL writes with a BC suffix rather than as a negative year")
	}
	return timestampLiteral(moment), nil
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

func numericLiteralValue(text string) (float64, bool, error) {
	lowered := strings.ToLower(text)
	if strings.HasPrefix(lowered, "0x") {
		value, err := strconv.ParseUint(lowered[2:], 16, 64)
		if err != nil {
			return 0, false, fmt.Errorf("%w: the hexadecimal literal %q does not fit in 64 bits", store.ErrInvalid, text)
		}
		return float64(int64(value)), true, nil
	}
	value, err := strconv.ParseFloat(text, 64)
	if err != nil {
		return 0, false, nil
	}
	return value, true, nil
}

func constantAtRenderTime(n filter.Node) bool {
	switch node := n.(type) {
	case *filter.Literal:
		return true
	case *filter.Param:
		return true
	case *filter.Unary:
		_, ok := node.Operand.(*filter.Literal)
		return ok
	}
	return false
}

func (r *filterRenderer) literalMoment(node *filter.Literal) (string, error) {
	switch node.Kind {
	case filter.LiteralNull:
		return "NULL::timestamp", nil
	case filter.LiteralString:
		return timeTextMoment(node.Text)
	case filter.LiteralNumber:
		value, ok, err := numericLiteralValue(node.Text)
		if err != nil {
			return "", err
		}
		if !ok {
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
	return quantizedToSQLitesMillisecond(timestampColumn(ident(physical))), nil
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
	return "", filterNotRenderable(node.Name + " inside another date or time function")
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
	shiftMillis := int64(0)
	for _, arg := range args[1:] {
		text, ok := r.constantText(arg)
		if !ok {
			if constantAtRenderTime(arg) {
				return "NULL::timestamp", nil
			}
			return "", filterNotRenderable("a date or time modifier this engine cannot read while building the statement")
		}
		seconds, support := parseSQLiteModifier(text)
		switch support {
		case sqliteOnly:
			return "", filterNotRenderable("the modifier " + strconv.Quote(text))
		case malformedInSQLite:
			return "NULL::timestamp", nil
		}
		millis := math.Round(seconds * 1000)
		if math.IsNaN(millis) || math.Abs(millis) > maxShiftMillis {
			return "NULL::timestamp", nil
		}
		shiftMillis += int64(millis)
		if shiftMillis > maxShiftMillis || shiftMillis < -maxShiftMillis {
			return "NULL::timestamp", nil
		}
	}
	if shiftMillis == 0 {
		return base, nil
	}
	low, high := shiftedBy(firstRenderableMoment, -shiftMillis), shiftedBy(lastRenderableMoment, -shiftMillis)
	if low.Before(firstRenderableMoment) {
		low = firstRenderableMoment
	}
	if high.After(lastRenderableMoment) {
		high = lastRenderableMoment
	}
	if high.Before(low) {
		return "NULL::timestamp", nil
	}
	shift := float64(shiftMillis) / 1000
	bounded := "(CASE WHEN " + base + " BETWEEN " + timestampLiteral(low) + " AND " + timestampLiteral(high) +
		" THEN " + base + " END)"
	return "(" + bounded + " + pg_catalog.make_interval(secs => " + strconv.FormatFloat(shift, 'f', -1, 64) + "))", nil
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
	case *filter.Unary:
		if lit, ok := node.Operand.(*filter.Literal); ok && lit.Kind == filter.LiteralNumber && (node.Op == "-" || node.Op == "+") {
			value, known, err := numericLiteralValue(lit.Text)
			if err != nil {
				return "", err
			}
			if !known {
				return "NULL::timestamp", nil
			}
			if node.Op == "-" {
				value = -value
			}
			return julianMoment(value)
		}
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
	moment = withinRepresentableYears(moment)
	if !ok {
		r.sb.WriteString(sqliteDouble("(pg_catalog.extract('epoch', " + moment + ") * 1000 + " +
			strconv.FormatInt(julianUnixEpochMillis, 10) + ")::float8 / 86400000::float8"))
		return nil
	}
	r.sb.WriteString("pg_catalog.to_char(" + moment + ", " + dollarQuote(pattern) + ")")
	return nil
}
