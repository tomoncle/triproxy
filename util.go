package main

import "time"

func timeNow() int64 {
	return time.Now().Unix()
}
