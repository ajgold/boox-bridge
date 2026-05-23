package dto

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/sysop/ultrabridge/internal/spcserver/envelope"
)

// TestCapacityVOFlat verifies CapacityVO serializes success/usedCapacity/
// totalCapacity at the top level (no "data" nesting) — the flat-envelope invariant.
func TestCapacityVOFlat(t *testing.T) {
	b, err := json.Marshal(CapacityVO{BaseVO: envelope.OK(), UsedCapacity: 400, TotalCapacity: 1 << 40})
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	s := string(b)
	for _, want := range []string{`"success":true`, `"usedCapacity":400`, `"totalCapacity":1099511627776`} {
		if !strings.Contains(s, want) {
			t.Errorf("CapacityVO JSON missing %q: %s", want, s)
		}
	}
	if strings.Contains(s, `"data"`) {
		t.Errorf("CapacityVO must not nest under data: %s", s)
	}
}

// TestCapacityLocalVOFlat verifies the get_space_usage VO is flat with a nested allocationVO.
func TestCapacityLocalVOFlat(t *testing.T) {
	b, _ := json.Marshal(CapacityLocalVO{
		BaseVO:       envelope.OK(),
		Used:         400,
		AllocationVO: AllocationVO{Tag: "individual", Allocated: 1 << 40},
		EquipmentNo:  "SN078",
	})
	s := string(b)
	for _, want := range []string{`"success":true`, `"used":400`, `"allocationVO":{`, `"allocated":1099511627776`, `"equipmentNo":"SN078"`} {
		if !strings.Contains(s, want) {
			t.Errorf("CapacityLocalVO JSON missing %q: %s", want, s)
		}
	}
}
