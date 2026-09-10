//go:build !linux

package processgroup

func reapGroupZombies(_, _ int) {}
