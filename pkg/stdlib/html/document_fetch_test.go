package html

import (
	"testing"
	"time"

	. "github.com/smartystreets/goconvey/convey"

	"github.com/MontFerret/ferret/pkg/drivers"
	"github.com/MontFerret/ferret/pkg/runtime/values"
)

func TestParseFetchParams(t *testing.T) {
	Convey("parseFetchParams", t, func() {
		Convey("Should return nil when fetchMode is omitted or false", func() {
			res, err := parseFetchParams(values.NewObjectWith(
				values.NewObjectProperty("fetchPoolKey", values.NewString("wb")),
			))
			So(err, ShouldBeNil)
			So(res, ShouldBeNil)

			res, err = parseFetchParams(values.NewObjectWith(
				values.NewObjectProperty("fetchMode", values.False),
				values.NewObjectProperty("fetchPoolSize", values.NewInt(3)),
			))
			So(err, ShouldBeNil)
			So(res, ShouldBeNil)
		})

		Convey("Should leave omitted fields zero for the driver to fill", func() {
			res, err := parseFetchParams(values.NewObjectWith(
				values.NewObjectProperty("fetchMode", values.True),
			))
			So(err, ShouldBeNil)
			So(*res, ShouldResemble, drivers.FetchParams{})
		})

		Convey("Should read every field and take fetchMaxAge in seconds", func() {
			res, err := parseFetchParams(values.NewObjectWith(
				values.NewObjectProperty("fetchMode", values.True),
				values.NewObjectProperty("fetchPoolKey", values.NewString("wb")),
				values.NewObjectProperty("fetchPoolSize", values.NewInt(2)),
				values.NewObjectProperty("fetchMaxUses", values.NewInt(100)),
				values.NewObjectProperty("fetchMaxAge", values.NewInt(60)),
			))
			So(err, ShouldBeNil)
			So(res.PoolKey, ShouldEqual, "wb")
			So(res.PoolSize, ShouldEqual, 2)
			So(res.MaxUses, ShouldEqual, 100)
			So(res.MaxAge, ShouldEqual, time.Minute)
		})

		Convey("Should reject fetchMode of a wrong type", func() {
			_, err := parseFetchParams(values.NewObjectWith(
				values.NewObjectProperty("fetchMode", values.NewString("true")),
			))
			So(err, ShouldNotBeNil)
		})

		Convey("Should reject pool settings of a wrong type", func() {
			for _, prop := range []*values.ObjectProperty{
				values.NewObjectProperty("fetchPoolKey", values.NewInt(1)),
				values.NewObjectProperty("fetchPoolSize", values.NewString("2")),
				values.NewObjectProperty("fetchMaxUses", values.NewFloat(1.5)),
				values.NewObjectProperty("fetchMaxAge", values.NewString("60")),
			} {
				obj := values.NewObjectWith(values.NewObjectProperty("fetchMode", values.True), prop)

				_, err := parseFetchParams(obj)
				So(err, ShouldNotBeNil)
			}
		})
	})
}
