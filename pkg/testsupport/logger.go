// Package testsupport provides shared doubles for albs' unit tests.
package testsupport

// Logger discards all output.
type Logger struct{}

func (Logger) Debugf(string, ...interface{}) {}
func (Logger) Infof(string, ...interface{})  {}
func (Logger) Warnf(string, ...interface{})  {}
