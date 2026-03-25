//go:build darwin

package vm

func NewManager(name string) (VMManager, error) {
	return NewLimaManager(name)
}
