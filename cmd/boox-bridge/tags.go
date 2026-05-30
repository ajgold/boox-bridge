package main

import (
	"archive/zip"
	"io"
	"strings"

	"google.golang.org/protobuf/encoding/protowire"
)

// extractBooxTags pulls tag names out of a Boox .note ZIP's tag/pb/* protobuf
// entries. Tags are stored as a repeated Tag message; only the name field
// (tag 7) is interesting for our purposes. Best-effort: any error returns
// an empty slice rather than failing the pipeline.
//
// Wire format observed on Note Air4C firmware (2026-05):
//
//	TagList {
//	  repeated Tag tags = 1;
//	}
//	Tag {
//	  string tag_uuid    = 1;
//	  string note_uuid   = 2;
//	  string page_uuid   = 3;
//	  string account_id  = 4;
//	  int64  created_ts  = 5;
//	  int64  modified_ts = 6;
//	  string name        = 7;
//	}
func extractBooxTags(notePath string) []string {
	zr, err := zip.OpenReader(notePath)
	if err != nil {
		return nil
	}
	defer zr.Close()

	seen := make(map[string]struct{})
	var out []string
	for _, f := range zr.File {
		if !strings.Contains(f.Name, "/tag/pb/") || strings.HasSuffix(f.Name, "/") {
			continue
		}
		rc, err := f.Open()
		if err != nil {
			continue
		}
		data, err := io.ReadAll(rc)
		rc.Close()
		if err != nil {
			continue
		}
		for _, name := range parseTagNames(data) {
			name = strings.TrimSpace(name)
			if name == "" {
				continue
			}
			if _, ok := seen[name]; ok {
				continue
			}
			seen[name] = struct{}{}
			out = append(out, name)
		}
	}
	return out
}

// parseTagNames walks the protobuf wire format of a TagList payload and
// returns every Tag.name (field 7) it finds.
func parseTagNames(data []byte) []string {
	var names []string
	for len(data) > 0 {
		num, typ, n := protowire.ConsumeTag(data)
		if n < 0 {
			return names
		}
		data = data[n:]
		if typ == protowire.BytesType {
			payload, m := protowire.ConsumeBytes(data)
			if m < 0 {
				return names
			}
			data = data[m:]
			// Outer field 1 = nested Tag message. Walk its fields.
			if num != 1 {
				continue
			}
			inner := payload
			for len(inner) > 0 {
				innerNum, innerTyp, k := protowire.ConsumeTag(inner)
				if k < 0 {
					break
				}
				inner = inner[k:]
				switch innerTyp {
				case protowire.BytesType:
					v, j := protowire.ConsumeBytes(inner)
					if j < 0 {
						break
					}
					inner = inner[j:]
					if innerNum == 7 {
						names = append(names, string(v))
					}
				case protowire.VarintType:
					_, j := protowire.ConsumeVarint(inner)
					if j < 0 {
						break
					}
					inner = inner[j:]
				case protowire.Fixed32Type:
					_, j := protowire.ConsumeFixed32(inner)
					if j < 0 {
						break
					}
					inner = inner[j:]
				case protowire.Fixed64Type:
					_, j := protowire.ConsumeFixed64(inner)
					if j < 0 {
						break
					}
					inner = inner[j:]
				default:
					return names
				}
			}
		} else {
			// Skip unknown top-level fields conservatively.
			switch typ {
			case protowire.VarintType:
				_, m := protowire.ConsumeVarint(data)
				if m < 0 {
					return names
				}
				data = data[m:]
			case protowire.Fixed32Type:
				_, m := protowire.ConsumeFixed32(data)
				if m < 0 {
					return names
				}
				data = data[m:]
			case protowire.Fixed64Type:
				_, m := protowire.ConsumeFixed64(data)
				if m < 0 {
					return names
				}
				data = data[m:]
			default:
				return names
			}
		}
		_ = num
	}
	return names
}
