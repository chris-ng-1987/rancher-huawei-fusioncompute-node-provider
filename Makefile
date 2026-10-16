# Root convenience targets. The Go driver also has its own Makefile in driver/.
.PHONY: driver ui test vet clean

## Build the Go driver binary into driver/.
driver:
	$(MAKE) -C driver build

## Build the Rancher UI extension bundle into ui/dist-pkg/.
ui:
	cd ui && corepack enable && yarn install --network-timeout 600000 && bash ./scripts/build-extension.sh

## Run the driver unit tests.
test:
	$(MAKE) -C driver test

## Vet the driver.
vet:
	$(MAKE) -C driver vet

clean:
	$(MAKE) -C driver clean
	rm -rf ui/dist ui/dist-pkg ui/.shell ui/pkg/fusioncompute/.shell
