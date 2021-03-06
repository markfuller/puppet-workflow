package puppet

import (
	"github.com/lyraproj/issue/issue"
	"github.com/lyraproj/puppet-evaluator/eval"
	"github.com/lyraproj/puppet-evaluator/impl"
	"github.com/lyraproj/puppet-evaluator/types"
	"github.com/lyraproj/puppet-parser/parser"
	"github.com/lyraproj/servicesdk/wfapi"
	"strings"
)

type PuppetActivity interface {
	Activity() wfapi.Activity

	Name() string
}

type puppetActivity struct {
	name       string
	parent     *puppetActivity
	expression parser.Expression
	properties eval.OrderedMap
	activity   wfapi.Activity
}

func init() {
	impl.NewPuppetActivity = func(c eval.Context, expression *parser.ActivityExpression) eval.Resolvable {
		return newActivity(c, nil, expression)
	}
}

func (a *puppetActivity) Activity() wfapi.Activity {
	return a.activity
}

func (a *puppetActivity) Name() string {
	return a.name
}

func (a *puppetActivity) Resolve(c eval.Context) {
	if a.activity == nil {
		switch a.Style() {
		case `stateHandler`:
			a.activity = wfapi.NewStateHandler(c, a.buildStateHandler)
		case `workflow`:
			a.activity = wfapi.NewWorkflow(c, a.buildWorkflow)
		case `resource`:
			a.activity = wfapi.NewResource(c, a.buildResource)
		case `action`:
			a.activity = wfapi.NewAction(c, a.buildAction)
		}
	}
}

func (a *puppetActivity) buildActivity(builder wfapi.Builder) {
	builder.Name(a.Name())
	builder.When(a.getWhen())
	builder.Input(a.extractParameters(a.properties, `input`, a.inferInput)...)
	builder.Output(a.extractParameters(a.properties, `output`, func() []eval.Parameter { return eval.NoParameters })...)
}

func newActivity(c eval.Context, parent *puppetActivity, ex *parser.ActivityExpression) *puppetActivity {
	ca := &puppetActivity{parent: parent, expression: ex}
	if props := ex.Properties(); props != nil {
		v := eval.Evaluate(c, props)
		dh, ok := v.(*types.HashValue)
		if !ok {
			panic(eval.Error(WF_FIELD_TYPE_MISMATCH, issue.H{`field`: `properties`, `expected`: `Hash`, `actual`: v.PType()}))
		}
		ca.properties = dh
	} else {
		ca.properties = eval.EMPTY_MAP
	}
	sgs := strings.Split(ex.Name(), `::`)
	ca.name = sgs[len(sgs)-1]
	return ca
}

func (a *puppetActivity) buildStateHandler(builder wfapi.StateHandlerBuilder) {
	a.buildActivity(builder)
	builder.API(a.getAPI(builder.Context(), builder.GetInput()))
}

func (a *puppetActivity) buildResource(builder wfapi.ResourceBuilder) {
	a.buildActivity(builder)
	c := builder.Context()
	builder.State(&state{ctx: c, stateType: a.getResourceType(c), unresolvedState: a.getState(c)})
	if extId, ok := a.getStringProperty(`externalId`); ok {
		builder.ExternalId(extId)
	}
}

func (a *puppetActivity) buildAction(builder wfapi.ActionBuilder) {
	if fd, ok := a.expression.(*parser.FunctionDefinition); ok {
		fn := impl.NewPuppetFunction(fd)
		fn.Resolve(builder.Context())
		builder.Name(fn.Name())
		builder.Input(fn.Parameters()...)
		builder.Doer(&do{name: fn.Name(), body: fd.Body(), parameters: fn.Parameters()})
		s := fn.Signature()
		rt := s.ReturnType()
		if rt != nil {
			if st, ok := rt.(*types.StructType); ok {
				es := st.Elements()
				ps := make([]eval.Parameter, len(es))
				for i, e := range es {
					ps[i] = impl.NewParameter(e.Name(), e.Value(), nil, false)
				}
				builder.Output(ps...)
			}
		}
		return
	}
	if ae, ok := a.expression.(*parser.ActivityExpression); ok {
		a.buildActivity(builder)
		builder.Doer(&do{name: builder.GetName(), body: ae.Definition(), parameters: builder.GetInput()})
	}
}

func (a *puppetActivity) buildWorkflow(builder wfapi.WorkflowBuilder) {
	a.buildActivity(builder)
	de := a.expression.(*parser.ActivityExpression).Definition()
	if de == nil {
		return
	}

	block, ok := de.(*parser.BlockExpression)
	if !ok {
		panic(eval.Error(WF_FIELD_TYPE_MISMATCH, issue.H{`field`: `definition`, `expected`: `CodeBlock`, `actual`: de}))
	}

	// Block should only contain activity expressions or something is wrong.
	for _, stmt := range block.Statements() {
		if as, ok := stmt.(*parser.ActivityExpression); ok {
			a.workflowActivity(builder, as)
		} else if fn, ok := stmt.(*parser.FunctionDefinition); ok {
			ac := &puppetActivity{parent: a, expression: fn}
			builder.Action(ac.buildAction)
		} else {
			panic(eval.Error(WF_NOT_ACTIVITY, issue.H{`actual`: stmt}))
		}
	}
}

func (a *puppetActivity) workflowActivity(builder wfapi.WorkflowBuilder, as *parser.ActivityExpression) {
	ac := newActivity(builder.Context(), a, as)
	if _, ok := ac.properties.Get4(`iteration`); ok {
		builder.Iterator(ac.buildIterator)
	} else {
		switch as.Style() {
		case parser.ActivityStyleStateHandler:
			builder.StateHandler(ac.buildStateHandler)
		case parser.ActivityStyleWorkflow:
			builder.Workflow(ac.buildWorkflow)
		case parser.ActivityStyleResource:
			builder.Resource(ac.buildResource)
		case parser.ActivityStyleAction:
			builder.Action(ac.buildAction)
		}
	}
}

func (a *puppetActivity) Style() string {
	if _, ok := a.expression.(*parser.FunctionDefinition); ok {
		return `action`
	}
	return string(a.expression.(*parser.ActivityExpression).Style())
}

func (a *puppetActivity) inferInput() []eval.Parameter {
	// TODO:
	return eval.NoParameters
}

func (a *puppetActivity) inferOutput() []eval.Parameter {
	// TODO:
	return eval.NoParameters
}

func noParamsFunc() []eval.Parameter {
	return eval.NoParameters
}

func (a *puppetActivity) buildIterator(builder wfapi.IteratorBuilder) {
	v, _ := a.properties.Get4(`iteration`)
	iteratorDef, ok := v.(*types.HashValue)
	if !ok {
		panic(eval.Error(WF_FIELD_TYPE_MISMATCH, issue.H{`field`: `iteration`, `expected`: `Hash`, `actual`: v.PType()}))
	}

	v = iteratorDef.Get5(`function`, eval.UNDEF)
	style, ok := v.(eval.StringValue)
	if !ok {
		panic(eval.Error(WF_FIELD_TYPE_MISMATCH, issue.H{`field`: `iteration.style`, `expected`: `String`, `actual`: v}))
	}
	if name, ok := iteratorDef.Get4(`name`); ok {
		builder.Name(name.String())
	}
	builder.Style(wfapi.NewIterationStyle(style.String()))
	builder.Over(a.extractParameters(iteratorDef, `params`, noParamsFunc)...)
	builder.Variables(a.extractParameters(iteratorDef, `vars`, noParamsFunc)...)

	switch a.Style() {
	case `stateHandler`:
		builder.StateHandler(a.buildStateHandler)
	case `workflow`:
		builder.Workflow(a.buildWorkflow)
	case `resource`:
		builder.Resource(a.buildResource)
	case `action`:
		builder.Action(a.buildAction)
	}
}

func (a *puppetActivity) getAPI(c eval.Context, input []eval.Parameter) eval.PuppetObject {
	var de parser.Expression
	if ae, ok := a.expression.(*parser.ActivityExpression); ok {
		de = ae.Definition()
	} else {
		// The block is the function
		return NewDo(a.Name(), input, a.expression)
	}
	if de == nil {
		panic(c.Error(a.expression, WF_NO_DEFINITION, issue.NO_ARGS))
	}

	block, ok := de.(*parser.BlockExpression)
	if !ok {
		panic(c.Error(de, WF_FIELD_TYPE_MISMATCH, issue.H{`field`: `definition`, `expected`: `CodeBlock`, `actual`: de}))
	}

	// Block must only consist of functions the functions create, read, update, and delete.
	var create, read, update, remove eval.InvocableValue
	for _, e := range block.Statements() {
		if fd, ok := e.(*parser.FunctionDefinition); ok {
			switch fd.Name() {
			case `create`:
				create = createFunction(c, fd)
				continue
			case `read`:
				read = createFunction(c, fd)
				continue
			case `update`:
				update = createFunction(c, fd)
				continue
			case `delete`:
				remove = createFunction(c, fd)
				continue
			default:
				panic(c.Error(e, WF_INVALID_FUNCTION, issue.H{`name`: fd.Name()}))
			}
		}
		panic(c.Error(e, WF_FIELD_TYPE_MISMATCH, issue.H{`field`: `definition`, `expected`: `function`, `actual`: e}))
	}

	missing := ``
	if create == nil {
		missing = `create`
	} else if read == nil {
		missing = `read`
	} else if remove == nil {
		missing = `delete`
	}
	if missing != `` {
		panic(c.Error(block, WF_MISSING_REQUIRED_FUNCTION, issue.H{`function`: missing}))
	}
	if update == nil {
		return NewCRD(a.Name(), create, read, remove)
	}
	return NewCRUD(a.Name(), create, read, update, remove)
}

func createFunction(c eval.Context, fd *parser.FunctionDefinition) impl.PuppetFunction {
	f := impl.NewPuppetFunction(fd)
	f.Resolve(c)
	return f
}

func (a *puppetActivity) getWhen() string {
	if when, ok := a.getStringProperty(`when`); ok {
		return when
	}
	return ``
}

func (a *puppetActivity) extractParameters(props eval.OrderedMap, field string, dflt func() []eval.Parameter) []eval.Parameter {
	if props == nil {
		return dflt()
	}

	v, ok := props.Get4(field)
	if !ok {
		return dflt()
	}

	ia, ok := v.(*types.ArrayValue)
	if !ok {
		panic(eval.Error(WF_FIELD_TYPE_MISMATCH, issue.H{`field`: field, `expected`: `Array`, `actual`: v.PType()}))
	}

	params := make([]eval.Parameter, ia.Len())
	ia.EachWithIndex(func(v eval.Value, i int) {
		if p, ok := v.(eval.Parameter); ok {
			params[i] = p
		} else {
			panic(eval.Error(WF_ELEMENT_NOT_PARAMETER, issue.H{`type`: p.PType(), `field`: field}))
		}
	})
	return params
}

func (a *puppetActivity) getState(c eval.Context) eval.OrderedMap {
	ae, ok := a.expression.(*parser.ActivityExpression)
	if !ok {
		return eval.EMPTY_MAP
	}
	de := ae.Definition()
	if de == nil {
		return eval.EMPTY_MAP
	}

	if hash, ok := de.(*parser.LiteralHash); ok {
		// Transform all variable references to Deferred expressions
		return eval.Evaluate(c, hash).(eval.OrderedMap)
	}
	panic(eval.Error(WF_FIELD_TYPE_MISMATCH, issue.H{`field`: `definition`, `expected`: `Hash`, `actual`: de}))
}

func (a *puppetActivity) getResourceType(c eval.Context) eval.ObjectType {
	n := a.Name()
	if a.properties != nil {
		if tv, ok := a.properties.Get4(`type`); ok {
			if t, ok := tv.(eval.ObjectType); ok {
				return t
			}
			if s, ok := tv.(eval.StringValue); ok {
				n = s.String()
			} else {
				panic(eval.Error(WF_FIELD_TYPE_MISMATCH, issue.H{`field`: `definition`, `expected`: `Variant[String,ObjectType]`, `actual`: tv}))
			}
		} else {
			ts := a.getTypeSpace()
			if ts != `` {
				n = ts + `::` + wfapi.LeafName(n)
			}
		}
	}
	tn := eval.NewTypedName(eval.NsType, n)
	if t, ok := eval.Load(c, tn); ok {
		if pt, ok := t.(eval.ObjectType); ok {
			return pt
		}
		panic(eval.Error(WF_FIELD_TYPE_MISMATCH, issue.H{`field`: `definition`, `expected`: `ObjectType`, `actual`: t}))
	}
	panic(eval.Error(eval.EVAL_UNRESOLVED_TYPE, issue.H{`typeString`: tn.Name()}))
}

func (a *puppetActivity) getTypeSpace() string {
	if ts, ok := a.getStringProperty(`typespace`); ok {
		return ts
	}
	if a.parent != nil {
		return a.parent.getTypeSpace()
	}
	return ``
}

func (a *puppetActivity) getStringProperty(field string) (string, bool) {
	if a.properties == nil {
		return ``, false
	}

	v, ok := a.properties.Get4(field)
	if !ok {
		return ``, false
	}

	if s, ok := v.(eval.StringValue); ok {
		return s.String(), true
	}
	panic(eval.Error(WF_FIELD_TYPE_MISMATCH, issue.H{`field`: field, `expected`: `String`, `actual`: v.PType()}))
}
