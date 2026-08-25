package model

const (
	// TemplateLiteralKey marks a value that resolves to its contents verbatim.
	// Reference-shaped data underneath it is data, not a reference.
	TemplateLiteralKey = "$literal"
	// TemplateFromKey names the namespace a reference object reads from.
	TemplateFromKey = "$from"
)

// WalkTemplateRefs visits every object in a template value, following the same
// short-circuits the resolver applies. A map carrying TemplateLiteralKey ends
// the walk of its subtree, because the resolver returns that value verbatim
// without evaluating anything inside it. A reference object likewise ends its
// subtree after being visited: the resolver reads only the reference's own
// scalar fields and never evaluates nested values, so anything below one is
// inert data rather than a reference the document actually has. visit returns
// false to stop the walk early.
//
// Reference analysis lives here rather than in each consumer so that dependency
// scheduling, run-ID scoping, and static verification cannot disagree about
// which references a document actually has.
func WalkTemplateRefs(v any, visit func(obj map[string]any) bool) {
	walkTemplateRefs(v, visit)
}

func walkTemplateRefs(v any, visit func(map[string]any) bool) bool {
	switch t := v.(type) {
	case map[string]any:
		if _, ok := t[TemplateLiteralKey]; ok {
			return true
		}
		if !visit(t) {
			return false
		}
		if _, ok := TemplateRefSource(t); ok {
			return true
		}
		for _, child := range t {
			if !walkTemplateRefs(child, visit) {
				return false
			}
		}
	case []any:
		for _, child := range t {
			if !walkTemplateRefs(child, visit) {
				return false
			}
		}
	}
	return true
}

// TemplateRefSource returns the namespace a reference object reads from, and
// whether obj is a reference object at all.
func TemplateRefSource(obj map[string]any) (string, bool) {
	from, ok := obj[TemplateFromKey].(string)
	return from, ok
}
