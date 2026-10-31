package jobnotify

import (
	"bytes"
	"encoding/json"
	"fmt"
	"reflect"
	"strings"
	"time"
)

const metadataStringBytes = 8 << 10
const metadataTotalBytes = 32 << 10

type metadataField struct {
	name, source, format string
	value                reflect.Value
}

// Only compile-time tagged Job fields are projected; no arbitrary raw keys
// become protocol fields or influence recipient identity.
func metadataFields(job *Job) []metadataField {
	v := reflect.ValueOf(job).Elem()
	t := v.Type()
	fields := make([]metadataField, 0, t.NumField())
	for i := 0; i < t.NumField(); i++ {
		field := t.Field(i)
		if source := field.Tag.Get("slurm"); source != "" {
			fields = append(fields, metadataField{strings.Split(field.Tag.Get("json"), ",")[0], source, field.Tag.Get("format"), v.Field(i)})
		}
	}
	return fields
}

func metadataOmitted(job *Job, name string) {
	complete := false
	job.MetadataComplete = &complete
	path := "job." + name
	for _, existing := range job.MetadataOmitted {
		if existing == path {
			return
		}
	}
	job.MetadataOmitted = append(job.MetadataOmitted, path)
}

func noMetadataValue(raw json.RawMessage) bool {
	raw = bytes.TrimSpace(raw)
	return len(raw) == 0 || bytes.Equal(raw, []byte("null"))
}

func noValMetadata(raw json.RawMessage) (unknown, infinite bool) {
	var value struct {
		Set      *bool `json:"set"`
		Infinite bool  `json:"infinite"`
	}
	if json.Unmarshal(raw, &value) == nil && value.Set != nil {
		return !*value.Set || value.Infinite, value.Infinite
	}
	return false, false
}

func projectJobMetadata(job *Job, selected map[string]json.RawMessage) {
	complete := true
	job.MetadataComplete = &complete
	for _, field := range metadataFields(job) {
		raw := selected[field.source]
		if noMetadataValue(raw) {
			continue
		}
		var text string
		if field.format != "" || field.value.Type().Elem().Kind() == reflect.Int64 {
			unknown, infinite := noValMetadata(raw)
			if field.format == "minutes" && infinite {
				text = "UNLIMITED"
			} else {
				if unknown {
					continue
				}
				n, ok := number(raw)
				if !ok || n < 0 || n > (1<<53)-1 {
					metadataOmitted(job, field.name)
					continue
				}
				if field.format == "timestamp" {
					if n == 0 {
						continue
					}
					if n > 253402300799 {
						metadataOmitted(job, field.name)
						continue
					}
					text = time.Unix(n, 0).UTC().Format(time.RFC3339)
				} else if field.format == "minutes" {
					text = fmt.Sprintf("%d min", n)
				} else {
					value := reflect.New(field.value.Type().Elem())
					value.Elem().SetInt(n)
					field.value.Set(value)
					continue
				}
			}
		} else if json.Unmarshal(raw, &text) != nil {
			metadataOmitted(job, field.name)
			continue
		}
		// Never truncate a command, output path or TRES into a plausible but
		// incorrect value. Keep the raw record independently whenever it fits.
		if len([]byte(text)) > metadataStringBytes {
			metadataOmitted(job, field.name)
			continue
		}
		value := reflect.New(field.value.Type().Elem())
		value.Elem().SetString(text)
		field.value.Set(value)
	}
	trimJobMetadata(job, func() bool {
		fields := map[string]any{"metadata_complete": *job.MetadataComplete}
		if len(job.MetadataOmitted) != 0 {
			fields["metadata_omitted"] = job.MetadataOmitted
		}
		for _, field := range metadataFields(job) {
			if !field.value.IsNil() {
				fields[field.name] = field.value.Interface()
			}
		}
		encoded, _ := json.Marshal(fields)
		return len(encoded) <= metadataTotalBytes
	})
}

// Lower-priority optional metadata is removed only after the raw snapshots.
// This is also used for configurations smaller than the default 256KiB wire
// budget. Re-encoding a retained event is idempotent, including omission names.
func trimJobMetadata(job *Job, fits func() bool) {
	fields := metadataFields(job)
	for i := len(fields) - 1; i >= 0 && !fits(); i-- {
		if fields[i].value.IsNil() {
			continue
		}
		fields[i].value.SetZero()
		metadataOmitted(job, fields[i].name)
	}
}
