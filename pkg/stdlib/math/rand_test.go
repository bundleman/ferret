package math_test

import (
	"context"
	"testing"

	"github.com/MontFerret/ferret/pkg/stdlib/math"
	. "github.com/smartystreets/goconvey/convey"
)

func TestRand(t *testing.T) {
	Convey("Should return pseudo-random value", t, func() {
		out, err := math.Rand(context.Background())

		So(err, ShouldBeNil)
		So(out, ShouldBeLessThan, 1)
	})
}
