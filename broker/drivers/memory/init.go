package memorybroker

import (
	"github.com/customr/wsify/broker"
)

const name = "memory"

func init() {
	broker.Register(name, &Driver{})
}
