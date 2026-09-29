module jellymesh/lab/fusespike

go 1.27.1

require github.com/hanwen/go-fuse/v2 v2.11.0

require golang.org/x/sys v0.28.0 // indirect
replace github.com/hanwen/go-fuse/v2 => ./third_party/go-fuse
