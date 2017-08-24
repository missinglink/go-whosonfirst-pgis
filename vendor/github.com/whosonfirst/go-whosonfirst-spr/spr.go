package spr

import (
	"github.com/whosonfirst/go-whosonfirst-flags"
)

type StandardPlacesResult interface {
	Id() int64
	ParentId() int64
	Name() string
	Placetype() string
	Country() string
	Repo() string
	Path() string
	URI() string
	IsCurrent() flags.ExistentialFlag
	IsCeased() flags.ExistentialFlag
	IsDeprecated() flags.ExistentialFlag
	IsSuperseded() flags.ExistentialFlag
	IsSuperseding() flags.ExistentialFlag
	SupersededBy() []int64
	Supersedes() []int64
}

type Pagination interface {
	Pages() int
	Page() int
	PerPage() int
	Total() int
	Cursor() string
	NextQuery() string
}

type StandardPlacesResults interface {
	Results() []StandardPlacesResult
}