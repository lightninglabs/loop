# Docker


## Prerequisites

The only prerequisites are:
1. This repo, and
2. Docker

Building the docker image pulls in all the dev dependencies to build loop within the image itself. Having a `go` development environment is not required.


## Building the Docker Image

The docker image can be built using this command within the `loop` directory:
```
docker build --tag loop .
```
This command pulls down a `go` build container, builds `loop` and `loopd` executables, then publishes those binaries to a fresh, smaller image, and marks that image with the tag 'loop'.


## Running the Docker Image

The docker image contains:
* The binary `loopd`, at `/go/bin/loopd`
* The binary `loop`, at `/go/bin/loop`

Docker is very flexible so you can use that information however you choose. This guide isn't meant to be prescriptive.


### Example: Running loopd

One way of running `loopd` is 
```
docker run --rm -it --name loopd -v $HOME/.lnd:/root/.lnd -v $HOME/.loop:/root/.loop loop:latest loopd --network=testnet --lnd.host <my-lnd-ip-address>:10009
```

Things to note from this docker command:
* You can stop the server with Control-C, and it'll clean up the associated stopped container automatically.
* The name of the running container is 'loopd' (which you may need to know to run the `loop` command).
* The '.lnd' directory in your home directory is mapped into the container, and `loopd` will look for your tls.cert and macaroon in the default locations. If this isn't appropriate for your case you can map whatever directories you choose and override where `loopd` looks for them using additional command-line parameters.
* The '.loop' directory in your home directory is mapped into the container, and `loopd` will use that directory to store some state.
* You probably need to specify your LND server host and port explicitly, since by default `loopd` looks for it on localhost and there is no LND server on localhost within the container.
* No ports are mapped, so it's not possible to connect to the running `loopd` from outside the container. (This is deliberate. You can map ports 8081 and 11010 to connect from outside the container if you choose.)


### Example: Running loop

If you're using the example above to run `loopd`, you can then run the `loop` command inside that running container to execute loops. One way would be:
```
docker exec -it loopd loop out --channel <channel-id-you-want-to-use> --amt <amount-you-want-to-loop-out>
```

Things to note about this docker command:
* `docker exec` runs a command on an already-running container. In this case `docker exec loopd` says effectively 'run the rest of this command-line as a command on the already-running container 'loopd'.
* The `-it` flags tell docker to run the command interatively and act like it's using a terminal. This helps with commands that do more than just write to stdout.
* The remainder `loop out --channel <channel-id-you-want-to-use> --amt <amount-you-want-to-loop-out>` is the actual loop command you want to run. All the regular `loop` documentation applies to this bit.


### A Handy Script

If you're using the example above to run `loopd`, creating a script can simplify running `loop`.

Create a file with the following contents:
```
#!/usr/bin/env bash
TERMINAL_FLAGS=
if [ -t 1 ] ; then
        TERMINAL_FLAGS="-it"
fi

docker exec $TERMINAL_FLAGS loopd loop "${@}"
```
Call this script 'loop', put it somewhere in your $PATH, and make it executable. Then you can just run commands like:
```
loop out --channel <channel-id-you-want-to-use> --amt <amount-you-want-to-loop-out>
```
without having to remember (or use) the docker part explicitly.


## Caveats

Running `loopd` the way shown above won't restart `loopd` if it is stopped or if the computer is restarted. You may want to investigate running the 'loop' container at startup, or when your LND server starts. (For example, `docker` has restart options, or grouping of containers via `docker-compose`.)