module github.com/rhun-editor/tests

go 1.25

require github.com/rhun-editor/extension-protocol v1.1.0

// the SDK with the tests feature (protocol 1.1) is not published yet: the integration checkout beside
// this repository
replace github.com/rhun-editor/extension-protocol => ../extension-protocol
