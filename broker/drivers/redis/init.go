package redisbroker

import (
	"github.com/customr/wsify/broker"
)

const name = "redis"

func init() {
	broker.Register(name, &Driver{})
}
