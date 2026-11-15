// Decoding Grafana data frames (the dataplane JSON of /api/ds/query and the
// alert state history API) into long rows.
//
// A frame is a set of equal-length columns ("fields"). Its JSON form is
// {schema: {name, refId, meta, fields: [{name, type, labels, config}]},
//  data: {values: [[...], ...], entities: [...], nanos: [...]}}. Number
// fields encode NaN, +Inf and -Inf out of band in data.entities; time
// fields are epoch milliseconds with optional extra nanoseconds in
// data.nanos.

package grafana

import (
	"encoding/json"
	"fmt"
	"math"
	"strconv"
	"time"
)

type frameField struct {
	Name     string            `json:"name"`
	Type     string            `json:"type"`
	Labels   map[string]string `json:"labels"`
	Config   map[string]any    `json:"config"`
	TypeInfo struct {
		Frame string `json:"frame"`
	} `json:"typeInfo"`
}

type frameEntities struct {
	NaN       []int `json:"NaN"`
	Inf       []int `json:"Inf"`
	NegInf    []int `json:"NegInf"`
	Undefined []int `json:"Undefined"`
}

type dataFrame struct {
	Schema struct {
		Name   string         `json:"name"`
		RefID  string         `json:"refId"`
		Meta   map[string]any `json:"meta"`
		Fields []frameField   `json:"fields"`
	} `json:"schema"`
	Data struct {
		Values   [][]json.RawMessage `json:"values"`
		Entities []*frameEntities    `json:"entities"`
		Nanos    [][]int64           `json:"nanos"`
	} `json:"data"`
}

// longRow is one decoded cell: a non-time field's value at one row.
type longRow struct {
	RefID      string
	FrameIndex int64
	FrameName  string
	FrameType  string
	Field      string
	Labels     map[string]string
	RowIndex   int64
	TS         any // time.Time or nil
	Value      any // float64 or nil
	Str        any // string or nil
	Unit       any
}

// frameTime decodes a time cell. Grafana sends epoch milliseconds; a few
// APIs send microseconds or nanoseconds, recognised by magnitude.
func frameTime(raw json.RawMessage, nanos int64) any {
	var f float64
	if err := json.Unmarshal(raw, &f); err != nil {
		var s string
		if json.Unmarshal(raw, &s) == nil {
			return parseTime(s)
		}
		return nil
	}
	v := int64(f)
	switch {
	case math.Abs(f) >= 1e17:
		return time.Unix(0, v).UTC()
	case math.Abs(f) >= 1e14:
		return time.UnixMicro(v).UTC()
	}
	return time.UnixMilli(v).Add(time.Duration(nanos)).UTC()
}

// decodeFrame turns one frame into long rows: one per (row, non-time field),
// with ts from the frame's first time field.
func decodeFrame(fr *dataFrame, refID string, index int) ([]longRow, error) {
	fields := fr.Schema.Fields
	if len(fr.Data.Values) != 0 && len(fr.Data.Values) != len(fields) {
		return nil, fmt.Errorf("frame %d of %s: %d fields but %d value columns", index, refID, len(fields), len(fr.Data.Values))
	}
	if fr.Schema.RefID != "" {
		refID = fr.Schema.RefID
	}
	frameType, _ := fr.Schema.Meta["type"].(string)
	timeIdx := -1
	for i, f := range fields {
		if f.Type == "time" {
			timeIdx = i
			break
		}
	}
	nrows := 0
	for _, col := range fr.Data.Values {
		nrows = max(nrows, len(col))
	}
	nanosAt := func(fi, ri int) int64 {
		if fi < len(fr.Data.Nanos) && ri < len(fr.Data.Nanos[fi]) {
			return fr.Data.Nanos[fi][ri]
		}
		return 0
	}
	var out []longRow
	for fi, f := range fields {
		if fi == timeIdx {
			continue
		}
		special := map[int]float64{}
		if fi < len(fr.Data.Entities) && fr.Data.Entities[fi] != nil {
			e := fr.Data.Entities[fi]
			for _, i := range e.NaN {
				special[i] = math.NaN()
			}
			for _, i := range e.Undefined {
				special[i] = math.NaN()
			}
			for _, i := range e.Inf {
				special[i] = math.Inf(1)
			}
			for _, i := range e.NegInf {
				special[i] = math.Inf(-1)
			}
		}
		unit, _ := f.Config["unit"].(string)
		labels := f.Labels
		if labels == nil {
			labels = map[string]string{}
		}
		name := f.Name
		if dn, ok := f.Config["displayNameFromDS"].(string); ok && dn != "" {
			name = dn
		}
		var vals []json.RawMessage
		if fi < len(fr.Data.Values) {
			vals = fr.Data.Values[fi]
		}
		for ri := 0; ri < nrows; ri++ {
			r := longRow{RefID: refID, FrameIndex: int64(index), FrameName: fr.Schema.Name, FrameType: frameType,
				Field: name, Labels: labels, RowIndex: int64(ri), Unit: nonEmpty(unit)}
			if timeIdx >= 0 && timeIdx < len(fr.Data.Values) && ri < len(fr.Data.Values[timeIdx]) {
				r.TS = frameTime(fr.Data.Values[timeIdx][ri], nanosAt(timeIdx, ri))
			}
			var raw json.RawMessage
			if ri < len(vals) {
				raw = vals[ri]
			}
			if v, ok := special[ri]; ok {
				r.Value = v
			} else if len(raw) > 0 && string(raw) != "null" {
				switch f.Type {
				case "number":
					var x float64
					if err := json.Unmarshal(raw, &x); err == nil {
						r.Value = x
					}
				case "boolean":
					var b bool
					if err := json.Unmarshal(raw, &b); err == nil {
						r.Value = map[bool]float64{true: 1, false: 0}[b]
						r.Str = strconv.FormatBool(b)
					}
				case "string", "enum":
					var s string
					if err := json.Unmarshal(raw, &s); err == nil {
						r.Str = s
					} else {
						r.Str = string(raw)
					}
				case "time":
					if t, ok := frameTime(raw, nanosAt(fi, ri)).(time.Time); ok {
						r.Str = t.Format(time.RFC3339Nano)
					}
				default:
					r.Str = string(raw)
				}
			}
			out = append(out, r)
		}
	}
	return out, nil
}
