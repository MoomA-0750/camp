package query

import "errors"

// ErrBadBy は UsageSummary の by が知らない値だったとき。
var ErrBadBy = errors.New("by は day / model / session / project のいずれか")
