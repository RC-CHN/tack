module tack.local/management/integration

go 1.25.0

require (
	tack.local/management/control v0.0.0
	tack.local/management/node v0.0.0
	tack.local/management/protocol v0.0.0
)

replace tack.local/management/node => ../node

replace tack.local/management/control => ../control

replace tack.local/management/protocol => ../protocol
