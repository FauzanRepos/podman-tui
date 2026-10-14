package quadlet

import "errors"

type boundedOutput struct{ data []byte }

func (o *boundedOutput) Write(p []byte) (int, error) {
	if len(o.data)+len(p) > 1<<20 {
		return 0, errors.New("systemd/generator output exceeds limit")
	}
	o.data = append(o.data, p...)
	return len(p), nil
}
