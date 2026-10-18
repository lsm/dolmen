package store

import "context"

type openDirKey struct{}

func WithOpenDir(ctx context.Context, dir string) context.Context {
	return context.WithValue(ctx, openDirKey{}, dir)
}

func OpenDirFrom(ctx context.Context) string {
	dir, _ := ctx.Value(openDirKey{}).(string)
	return dir
}
