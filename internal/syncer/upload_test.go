package syncer

import (
	"reflect"
	"testing"

	"mrorsync/internal/baidu"
)

func TestMissingBlocks(t *testing.T) {
	blocks := []string{"m0", "m1", "m2"}

	cases := []struct {
		name string
		pre  *baidu.Precreate
		want []int
	}{
		{
			name: "numeric indexes are the blocks to upload",
			pre:  &baidu.Precreate{BlockList: []any{float64(1)}},
			want: []int{1},
		},
		{
			name: "out of range index is ignored",
			pre:  &baidu.Precreate{BlockList: []any{float64(9)}},
			want: []int{},
		},
		{
			name: "md5 strings mean blocks already present",
			pre:  &baidu.Precreate{BlockList: []any{"m0", "m2"}},
			want: []int{1},
		},
		{
			name: "empty list uploads everything",
			pre:  &baidu.Precreate{},
			want: []int{0, 1, 2},
		},
	}
	for _, c := range cases {
		got := missingBlocks(c.pre, blocks)
		if !reflect.DeepEqual(got, c.want) {
			t.Fatalf("%s: got %v, want %v", c.name, got, c.want)
		}
	}
	if got := missingBlocks(&baidu.Precreate{}, nil); got != nil {
		t.Fatalf("empty file should have no blocks, got %v", got)
	}
}
