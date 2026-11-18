package service

import (
	"math"
	"testing"
)

func TestSnapLocation(t *testing.T) {
	lat, lng, acc := SnapLocation(6.9010, 79.8722)
	if acc != LocationGridMeters {
		t.Fatalf("accuracy %v", acc)
	}
	// Within one grid cell of the input, and two nearby points share a cell.
	if math.Abs(lat-6.9010) > 0.0014 || math.Abs(lng-79.8722) > 0.0014 {
		t.Fatalf("snapped too far: %v %v", lat, lng)
	}
	lat2, lng2, _ := SnapLocation(6.90101, 79.87221)
	if lat2 != lat || lng2 != lng {
		t.Fatal("points 1 m apart landed in different cells")
	}
}
