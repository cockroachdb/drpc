// Copyright (C) 2022 Storj Labs, Inc.
// See LICENSE for copying information.

package drpcpool

import (
	"fmt"
	"time"
)

// connState tracks a pooled connection and its in-flight stream count.
type connState[K comparable] struct {
	key    K
	val    Conn
	active int
	exp    *time.Timer // only ticking when active == 0
}

func (cs *connState[K]) String() string {
	return fmt.Sprintf("<cs %p k:%v active:%d closed:%v>",
		cs, cs.key, cs.active, closed(cs.val.Closed()))
}
