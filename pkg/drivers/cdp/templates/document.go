package templates

import "github.com/MontFerret/ferret/pkg/drivers/cdp/eval"

const domReady = `() => {
if (document.readyState === 'complete') {
	return true;
}

return null;
}`

func DOMReady() *eval.Function {
	return eval.F(domReady)
}

// domParsed is the end of HTML parsing of the current document: any readyState but loading.
// Unlike domReady it does not wait for complete, which may come tens of seconds later or never.
const domParsed = `() => {
if (document.readyState !== 'loading') {
	return true;
}

return null;
}`

func DOMParsed() *eval.Function {
	return eval.F(domParsed)
}

const getTitle = `() => document.title`

func GetTitle() *eval.Function {
	return eval.F(getTitle)
}

const getDocument = `() => document`

func GetDocument() *eval.Function {
	return eval.F(getDocument)
}
