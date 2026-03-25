//go:build linux

package vm

func NewManager(name string) (VMManager, error) {
	return NewDirectManager(name), nil
}
