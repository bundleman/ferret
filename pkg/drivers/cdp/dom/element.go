package dom

import (
	"context"
	"hash/fnv"
	"strings"
	"time"

	"github.com/mafredri/cdp"
	"github.com/mafredri/cdp/protocol/runtime"
	"github.com/pkg/errors"
	"github.com/rs/zerolog"
	"github.com/wI2L/jettison"

	"github.com/MontFerret/ferret/pkg/drivers"
	"github.com/MontFerret/ferret/pkg/drivers/cdp/eval"
	"github.com/MontFerret/ferret/pkg/drivers/cdp/events"
	"github.com/MontFerret/ferret/pkg/drivers/cdp/input"
	"github.com/MontFerret/ferret/pkg/drivers/cdp/templates"
	"github.com/MontFerret/ferret/pkg/drivers/common"
	"github.com/MontFerret/ferret/pkg/runtime/core"
	"github.com/MontFerret/ferret/pkg/runtime/logging"
	"github.com/MontFerret/ferret/pkg/runtime/values"
)

type HTMLElement struct {
	logger   zerolog.Logger
	client   *cdp.Client
	dom      *Manager
	input    *input.Manager
	eval     *eval.Runtime
	id       runtime.RemoteObjectID
	nodeType *common.LazyValue
	nodeName *common.LazyValue
}

func NewHTMLElement(
	logger zerolog.Logger,
	client *cdp.Client,
	domManager *Manager,
	input *input.Manager,
	exec *eval.Runtime,
	id runtime.RemoteObjectID,
) *HTMLElement {
	el := new(HTMLElement)
	el.logger = logging.
		WithName(logger.With(), "dom_element").
		Str("object_id", string(id)).
		Logger()
	el.client = client
	el.dom = domManager
	el.input = input
	el.eval = exec
	el.id = id
	el.nodeType = common.NewLazyValue(func(ctx context.Context) (core.Value, error) {
		return el.callEvalValue(ctx, func(id runtime.RemoteObjectID) *eval.Function {
			return templates.GetNodeType(id)
		})
	})
	el.nodeName = common.NewLazyValue(func(ctx context.Context) (core.Value, error) {
		return el.callEvalValue(ctx, func(id runtime.RemoteObjectID) *eval.Function {
			return templates.GetNodeName(id)
		})
	})

	return el
}

func (el *HTMLElement) RemoteID() runtime.RemoteObjectID {
	return el.id
}

// SetRemoteID is invoked by the refresh hook.
func (el *HTMLElement) SetRemoteID(id runtime.RemoteObjectID) {
	el.id = id
}

type elBuilder func(id runtime.RemoteObjectID) *eval.Function

// retry makes one extra attempt on a stale error after refreshing the context and id.
func (el *HTMLElement) retry(ctx context.Context, build elBuilder, run func(*eval.Function) error) error {
	err := run(build(el.id))

	if err == nil || !eval.IsStaleErr(err) {
		return err
	}

	if refreshErr := el.eval.RefreshContext(ctx); refreshErr != nil {
		return err
	}

	return run(build(el.id))
}

func (el *HTMLElement) callEval(ctx context.Context, build elBuilder) error {
	return el.retry(ctx, build, func(fn *eval.Function) error {
		return el.eval.Eval(ctx, fn)
	})
}

func (el *HTMLElement) callEvalValue(ctx context.Context, build elBuilder) (core.Value, error) {
	var out core.Value
	err := el.retry(ctx, build, func(fn *eval.Function) error {
		v, e := el.eval.EvalValue(ctx, fn)
		out = v
		return e
	})
	return out, err
}

func (el *HTMLElement) callEvalElement(ctx context.Context, build elBuilder) (core.Value, error) {
	var out core.Value
	err := el.retry(ctx, build, func(fn *eval.Function) error {
		v, e := el.eval.EvalElement(ctx, fn)
		out = v
		return e
	})
	return out, err
}

func (el *HTMLElement) waitRun(ctx context.Context, build elBuilder) error {
	task := events.NewEvalWaitTaskBuilder(
		el.eval,
		func() *eval.Function { return build(el.id) },
		events.DefaultPolling,
	)
	_, err := task.Run(ctx)
	return err
}

func (el *HTMLElement) callEvalElements(ctx context.Context, build elBuilder) (*values.Array, error) {
	var out *values.Array
	err := el.retry(ctx, build, func(fn *eval.Function) error {
		v, e := el.eval.EvalElements(ctx, fn)
		out = v
		return e
	})
	return out, err
}

func (el *HTMLElement) Close() error {
	return nil
}

func (el *HTMLElement) Type() core.Type {
	return drivers.HTMLElementType
}

func (el *HTMLElement) MarshalJSON() ([]byte, error) {
	return jettison.MarshalOpts(el.String(), jettison.NoHTMLEscaping())
}

func (el *HTMLElement) String() string {
	ctx, cancel := context.WithTimeout(context.Background(), time.Duration(drivers.DefaultWaitTimeout)*time.Millisecond)
	defer cancel()

	res, err := el.GetInnerHTML(ctx)

	if err != nil {
		el.logError(errors.Wrap(err, "HTMLElement.String"))

		return ""
	}

	return res.String()
}

func (el *HTMLElement) Compare(other core.Value) int64 {
	switch other.Type() {
	case drivers.HTMLElementType:
		other := other.(drivers.HTMLElement)

		return int64(strings.Compare(el.String(), other.String()))
	default:
		return drivers.Compare(el.Type(), other.Type())
	}
}

func (el *HTMLElement) Unwrap() interface{} {
	return el
}

func (el *HTMLElement) Hash() uint64 {
	h := fnv.New64a()

	h.Write([]byte(el.Type().String()))
	h.Write([]byte(":"))
	h.Write([]byte(el.id))

	return h.Sum64()
}

func (el *HTMLElement) Copy() core.Value {
	return values.None
}

func (el *HTMLElement) Iterate(_ context.Context) (core.Iterator, error) {
	return common.NewIterator(el)
}

func (el *HTMLElement) GetIn(ctx context.Context, path []core.Value) (core.Value, core.PathError) {
	return common.GetInElement(ctx, path, el)
}

func (el *HTMLElement) SetIn(ctx context.Context, path []core.Value, value core.Value) core.PathError {
	return common.SetInElement(ctx, path, el, value)
}

func (el *HTMLElement) GetValue(ctx context.Context) (core.Value, error) {
	return el.callEvalValue(ctx, func(id runtime.RemoteObjectID) *eval.Function {
		return templates.GetValue(id)
	})
}

func (el *HTMLElement) SetValue(ctx context.Context, value core.Value) error {
	return el.callEval(ctx, func(id runtime.RemoteObjectID) *eval.Function {
		return templates.SetValue(id, value)
	})
}

func (el *HTMLElement) GetNodeType(ctx context.Context) (values.Int, error) {
	out, err := el.nodeType.Read(ctx)

	if err != nil {
		return values.ZeroInt, err
	}

	return values.ToInt(out), nil
}

func (el *HTMLElement) GetNodeName(ctx context.Context) (values.String, error) {
	out, err := el.nodeName.Read(ctx)

	if err != nil {
		return values.EmptyString, err
	}

	return values.ToString(out), nil
}

func (el *HTMLElement) Length() values.Int {
	value, err := el.callEvalValue(context.Background(), func(id runtime.RemoteObjectID) *eval.Function {
		return templates.GetChildrenCount(id)
	})

	if err != nil {
		el.logError(err)

		return 0
	}

	return values.ToInt(value)
}

func (el *HTMLElement) GetStyles(ctx context.Context) (*values.Object, error) {
	out, err := el.callEvalValue(ctx, func(id runtime.RemoteObjectID) *eval.Function {
		return templates.GetStyles(id)
	})

	if err != nil {
		return values.NewObject(), err
	}

	return values.ToObject(ctx, out), nil
}

func (el *HTMLElement) GetStyle(ctx context.Context, name values.String) (core.Value, error) {
	return el.callEvalValue(ctx, func(id runtime.RemoteObjectID) *eval.Function {
		return templates.GetStyle(id, name)
	})
}

func (el *HTMLElement) SetStyles(ctx context.Context, styles *values.Object) error {
	return el.callEval(ctx, func(id runtime.RemoteObjectID) *eval.Function {
		return templates.SetStyles(id, styles)
	})
}

func (el *HTMLElement) SetStyle(ctx context.Context, name, value values.String) error {
	return el.callEval(ctx, func(id runtime.RemoteObjectID) *eval.Function {
		return templates.SetStyle(id, name, value)
	})
}

func (el *HTMLElement) RemoveStyle(ctx context.Context, names ...values.String) error {
	return el.callEval(ctx, func(id runtime.RemoteObjectID) *eval.Function {
		return templates.RemoveStyles(id, names)
	})
}

func (el *HTMLElement) GetAttributes(ctx context.Context) (*values.Object, error) {
	out, err := el.callEvalValue(ctx, func(id runtime.RemoteObjectID) *eval.Function {
		return templates.GetAttributes(id)
	})

	if err != nil {
		return values.NewObject(), err
	}

	return values.ToObject(ctx, out), nil
}

func (el *HTMLElement) GetAttribute(ctx context.Context, name values.String) (core.Value, error) {
	return el.callEvalValue(ctx, func(id runtime.RemoteObjectID) *eval.Function {
		return templates.GetAttribute(id, name)
	})
}

func (el *HTMLElement) SetAttributes(ctx context.Context, attrs *values.Object) error {
	return el.callEval(ctx, func(id runtime.RemoteObjectID) *eval.Function {
		return templates.SetAttributes(id, attrs)
	})
}

func (el *HTMLElement) SetAttribute(ctx context.Context, name, value values.String) error {
	return el.callEval(ctx, func(id runtime.RemoteObjectID) *eval.Function {
		return templates.SetAttribute(id, name, value)
	})
}

func (el *HTMLElement) RemoveAttribute(ctx context.Context, names ...values.String) error {
	return el.callEval(ctx, func(id runtime.RemoteObjectID) *eval.Function {
		return templates.RemoveAttributes(id, names)
	})
}

func (el *HTMLElement) GetChildNodes(ctx context.Context) (*values.Array, error) {
	return el.callEvalElements(ctx, func(id runtime.RemoteObjectID) *eval.Function {
		return templates.GetChildren(id)
	})
}

func (el *HTMLElement) GetChildNode(ctx context.Context, idx values.Int) (core.Value, error) {
	return el.callEvalElement(ctx, func(id runtime.RemoteObjectID) *eval.Function {
		return templates.GetChildByIndex(id, idx)
	})
}

func (el *HTMLElement) GetParentElement(ctx context.Context) (core.Value, error) {
	return el.callEvalElement(ctx, func(id runtime.RemoteObjectID) *eval.Function {
		return templates.GetParent(id)
	})
}

func (el *HTMLElement) GetPreviousElementSibling(ctx context.Context) (core.Value, error) {
	return el.callEvalElement(ctx, func(id runtime.RemoteObjectID) *eval.Function {
		return templates.GetPreviousElementSibling(id)
	})
}

func (el *HTMLElement) GetNextElementSibling(ctx context.Context) (core.Value, error) {
	return el.callEvalElement(ctx, func(id runtime.RemoteObjectID) *eval.Function {
		return templates.GetNextElementSibling(id)
	})
}

func (el *HTMLElement) QuerySelector(ctx context.Context, selector drivers.QuerySelector) (core.Value, error) {
	return el.callEvalElement(ctx, func(id runtime.RemoteObjectID) *eval.Function {
		return templates.QuerySelector(id, selector)
	})
}

func (el *HTMLElement) QuerySelectorAll(ctx context.Context, selector drivers.QuerySelector) (*values.Array, error) {
	return el.callEvalElements(ctx, func(id runtime.RemoteObjectID) *eval.Function {
		return templates.QuerySelectorAll(id, selector)
	})
}

func (el *HTMLElement) XPath(ctx context.Context, expression values.String) (result core.Value, err error) {
	return el.callEvalValue(ctx, func(id runtime.RemoteObjectID) *eval.Function {
		return templates.XPath(id, expression)
	})
}

func (el *HTMLElement) GetInnerText(ctx context.Context) (values.String, error) {
	out, err := el.callEvalValue(ctx, func(id runtime.RemoteObjectID) *eval.Function {
		return templates.GetInnerText(id)
	})

	if err != nil {
		return values.EmptyString, err
	}

	return values.ToString(out), nil
}

func (el *HTMLElement) SetInnerText(ctx context.Context, innerText values.String) error {
	return el.callEval(ctx, func(id runtime.RemoteObjectID) *eval.Function {
		return templates.SetInnerText(id, innerText)
	})
}

func (el *HTMLElement) GetInnerTextBySelector(ctx context.Context, selector drivers.QuerySelector) (values.String, error) {
	out, err := el.callEvalValue(ctx, func(id runtime.RemoteObjectID) *eval.Function {
		return templates.GetInnerTextBySelector(id, selector)
	})

	if err != nil {
		return values.EmptyString, err
	}

	return values.ToString(out), nil
}

func (el *HTMLElement) SetInnerTextBySelector(ctx context.Context, selector drivers.QuerySelector, innerText values.String) error {
	return el.callEval(ctx, func(id runtime.RemoteObjectID) *eval.Function {
		return templates.SetInnerTextBySelector(id, selector, innerText)
	})
}

func (el *HTMLElement) GetInnerTextBySelectorAll(ctx context.Context, selector drivers.QuerySelector) (*values.Array, error) {
	out, err := el.callEvalValue(ctx, func(id runtime.RemoteObjectID) *eval.Function {
		return templates.GetInnerTextBySelectorAll(id, selector)
	})

	if err != nil {
		return values.EmptyArray(), err
	}

	return values.ToArray(ctx, out), nil
}

func (el *HTMLElement) GetInnerHTML(ctx context.Context) (values.String, error) {
	out, err := el.callEvalValue(ctx, func(id runtime.RemoteObjectID) *eval.Function {
		return templates.GetInnerHTML(id)
	})

	if err != nil {
		return values.EmptyString, err
	}

	return values.ToString(out), nil
}

func (el *HTMLElement) SetInnerHTML(ctx context.Context, innerHTML values.String) error {
	return el.callEval(ctx, func(id runtime.RemoteObjectID) *eval.Function {
		return templates.SetInnerHTML(id, innerHTML)
	})
}

func (el *HTMLElement) GetInnerHTMLBySelector(ctx context.Context, selector drivers.QuerySelector) (values.String, error) {
	out, err := el.callEvalValue(ctx, func(id runtime.RemoteObjectID) *eval.Function {
		return templates.GetInnerHTMLBySelector(id, selector)
	})

	if err != nil {
		return values.EmptyString, err
	}

	return values.ToString(out), nil
}

func (el *HTMLElement) SetInnerHTMLBySelector(ctx context.Context, selector drivers.QuerySelector, innerHTML values.String) error {
	return el.callEval(ctx, func(id runtime.RemoteObjectID) *eval.Function {
		return templates.SetInnerHTMLBySelector(id, selector, innerHTML)
	})
}

func (el *HTMLElement) GetInnerHTMLBySelectorAll(ctx context.Context, selector drivers.QuerySelector) (*values.Array, error) {
	out, err := el.callEvalValue(ctx, func(id runtime.RemoteObjectID) *eval.Function {
		return templates.GetInnerHTMLBySelectorAll(id, selector)
	})

	if err != nil {
		return values.EmptyArray(), err
	}

	return values.ToArray(ctx, out), nil
}

func (el *HTMLElement) CountBySelector(ctx context.Context, selector drivers.QuerySelector) (values.Int, error) {
	out, err := el.callEvalValue(ctx, func(id runtime.RemoteObjectID) *eval.Function {
		return templates.CountBySelector(id, selector)
	})

	if err != nil {
		return values.ZeroInt, err
	}

	return values.ToInt(out), nil
}

func (el *HTMLElement) ExistsBySelector(ctx context.Context, selector drivers.QuerySelector) (values.Boolean, error) {
	out, err := el.callEvalValue(ctx, func(id runtime.RemoteObjectID) *eval.Function {
		return templates.ExistsBySelector(id, selector)
	})

	if err != nil {
		return values.False, err
	}

	return values.ToBoolean(out), nil
}

func (el *HTMLElement) WaitForElement(ctx context.Context, selector drivers.QuerySelector, when drivers.WaitEvent) error {
	return el.waitRun(ctx, func(id runtime.RemoteObjectID) *eval.Function {
		return templates.WaitForElement(id, selector, when)
	})
}

func (el *HTMLElement) WaitForElementAll(ctx context.Context, selector drivers.QuerySelector, when drivers.WaitEvent) error {
	return el.waitRun(ctx, func(id runtime.RemoteObjectID) *eval.Function {
		return templates.WaitForElementAll(id, selector, when)
	})
}

func (el *HTMLElement) WaitForClass(ctx context.Context, class values.String, when drivers.WaitEvent) error {
	return el.waitRun(ctx, func(id runtime.RemoteObjectID) *eval.Function {
		return templates.WaitForClass(id, class, when)
	})
}

func (el *HTMLElement) WaitForClassBySelector(ctx context.Context, selector drivers.QuerySelector, class values.String, when drivers.WaitEvent) error {
	return el.waitRun(ctx, func(id runtime.RemoteObjectID) *eval.Function {
		return templates.WaitForClassBySelector(id, selector, class, when)
	})
}

func (el *HTMLElement) WaitForClassBySelectorAll(ctx context.Context, selector drivers.QuerySelector, class values.String, when drivers.WaitEvent) error {
	return el.waitRun(ctx, func(id runtime.RemoteObjectID) *eval.Function {
		return templates.WaitForClassBySelectorAll(id, selector, class, when)
	})
}

func (el *HTMLElement) WaitForAttribute(
	ctx context.Context,
	name values.String,
	value core.Value,
	when drivers.WaitEvent,
) error {
	return el.waitRun(ctx, func(id runtime.RemoteObjectID) *eval.Function {
		return templates.WaitForAttribute(id, name, value, when)
	})
}

func (el *HTMLElement) WaitForAttributeBySelector(ctx context.Context, selector drivers.QuerySelector, name values.String, value core.Value, when drivers.WaitEvent) error {
	return el.waitRun(ctx, func(id runtime.RemoteObjectID) *eval.Function {
		return templates.WaitForAttributeBySelector(id, selector, name, value, when)
	})
}

func (el *HTMLElement) WaitForAttributeBySelectorAll(ctx context.Context, selector drivers.QuerySelector, name values.String, value core.Value, when drivers.WaitEvent) error {
	return el.waitRun(ctx, func(id runtime.RemoteObjectID) *eval.Function {
		return templates.WaitForAttributeBySelectorAll(id, selector, name, value, when)
	})
}

func (el *HTMLElement) WaitForStyle(ctx context.Context, name values.String, value core.Value, when drivers.WaitEvent) error {
	return el.waitRun(ctx, func(id runtime.RemoteObjectID) *eval.Function {
		return templates.WaitForStyle(id, name, value, when)
	})
}

func (el *HTMLElement) WaitForStyleBySelector(ctx context.Context, selector drivers.QuerySelector, name values.String, value core.Value, when drivers.WaitEvent) error {
	return el.waitRun(ctx, func(id runtime.RemoteObjectID) *eval.Function {
		return templates.WaitForStyleBySelector(id, selector, name, value, when)
	})
}

func (el *HTMLElement) WaitForStyleBySelectorAll(ctx context.Context, selector drivers.QuerySelector, name values.String, value core.Value, when drivers.WaitEvent) error {
	return el.waitRun(ctx, func(id runtime.RemoteObjectID) *eval.Function {
		return templates.WaitForStyleBySelectorAll(id, selector, name, value, when)
	})
}

func (el *HTMLElement) Click(ctx context.Context, count values.Int) error {
	return el.input.Click(ctx, el.id, int(count))
}

func (el *HTMLElement) ClickBySelector(ctx context.Context, selector drivers.QuerySelector, count values.Int) error {
	return el.input.ClickBySelector(ctx, el.id, selector, count)
}

func (el *HTMLElement) ClickBySelectorAll(ctx context.Context, selector drivers.QuerySelector, count values.Int) error {
	elements, err := el.QuerySelectorAll(ctx, selector)

	if err != nil {
		return err
	}

	elements.ForEach(func(value core.Value, idx int) bool {
		found := value.(*HTMLElement)

		if e := found.Click(ctx, count); e != nil {
			err = e
			return false
		}

		return true
	})

	return err
}

func (el *HTMLElement) Input(ctx context.Context, value core.Value, delay values.Int) error {
	name, err := el.GetNodeName(ctx)

	if err != nil {
		return err
	}

	if strings.ToLower(string(name)) != "input" {
		return core.Error(core.ErrInvalidOperation, "element is not an <input> element.")
	}

	return el.input.Type(ctx, el.id, input.TypeParams{
		Text:  value.String(),
		Clear: false,
		Delay: time.Duration(delay) * time.Millisecond,
	})
}

func (el *HTMLElement) InputBySelector(ctx context.Context, selector drivers.QuerySelector, value core.Value, delay values.Int) error {
	return el.input.TypeBySelector(ctx, el.id, selector, input.TypeParams{
		Text:  value.String(),
		Clear: false,
		Delay: time.Duration(delay) * time.Millisecond,
	})
}

func (el *HTMLElement) Press(ctx context.Context, keys []values.String, count values.Int) error {
	return el.input.Press(ctx, values.UnwrapStrings(keys), int(count))
}

func (el *HTMLElement) PressBySelector(ctx context.Context, selector drivers.QuerySelector, keys []values.String, count values.Int) error {
	return el.input.PressBySelector(ctx, el.id, selector, values.UnwrapStrings(keys), int(count))
}

func (el *HTMLElement) Clear(ctx context.Context) error {
	return el.input.Clear(ctx, el.id)
}

func (el *HTMLElement) ClearBySelector(ctx context.Context, selector drivers.QuerySelector) error {
	return el.input.ClearBySelector(ctx, el.id, selector)
}

func (el *HTMLElement) Select(ctx context.Context, value *values.Array) (*values.Array, error) {
	return el.input.Select(ctx, el.id, value)
}

func (el *HTMLElement) SelectBySelector(ctx context.Context, selector drivers.QuerySelector, value *values.Array) (*values.Array, error) {
	return el.input.SelectBySelector(ctx, el.id, selector, value)
}

func (el *HTMLElement) ScrollIntoView(ctx context.Context, options drivers.ScrollOptions) error {
	return el.input.ScrollIntoView(ctx, el.id, options)
}

func (el *HTMLElement) Focus(ctx context.Context) error {
	return el.input.Focus(ctx, el.id)
}

func (el *HTMLElement) FocusBySelector(ctx context.Context, selector drivers.QuerySelector) error {
	return el.input.FocusBySelector(ctx, el.id, selector)
}

func (el *HTMLElement) Blur(ctx context.Context) error {
	return el.input.Blur(ctx, el.id)
}

func (el *HTMLElement) BlurBySelector(ctx context.Context, selector drivers.QuerySelector) error {
	return el.input.BlurBySelector(ctx, el.id, selector)
}

func (el *HTMLElement) Hover(ctx context.Context) error {
	return el.input.MoveMouse(ctx, el.id)
}

func (el *HTMLElement) HoverBySelector(ctx context.Context, selector drivers.QuerySelector) error {
	return el.input.MoveMouseBySelector(ctx, el.id, selector)
}

func (el *HTMLElement) logError(err error) *zerolog.Event {
	return el.logger.
		Error().
		Timestamp().
		Str("objectID", string(el.id)).
		Err(err)
}
