# [pkg/core/consensus/reduction/secondstep](./pkg/core/consensus/reduction/secondstep)

Implementation of the second step of the reduction phase.

<!-- ToC start -->

## Contents

1. [Gotchas](#gotchas)

<!-- ToC end -->

## Gotchas

- Caution should be taken when deciding on what terms the timeout should be
  increased. A bug was caught where, on completely valid results, the timeout
  would still increase as the node was not part of the agreement committee. The
  issue was
  logged [here](https://github.com/dusk-network/dusk-blockchain/issues/700) and
  fixed in [this PR](https://github.com/dusk-network/dusk-blockchain/pull/650).

Copyright © 2018-2022 Dusk Network
[MIT Licence](https://github.com/dusk-network/dusk-blockchain/blob/master/LICENSE)