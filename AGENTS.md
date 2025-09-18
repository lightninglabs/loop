### Project Summary: Lightning Loop

**1. Core Purpose:**
Lightning Loop is a non-custodial service from Lightning Labs for bridging the Bitcoin mainnet (on-chain) and the Lightning Network (off-chain). It enables users to rebalance their channels by moving funds into or out of Lightning — without the need for closing and reopening them. The system is architected to be non-custodial, using atomic swaps to ensure neither the user nor the server can lose funds.

**2. Architecture & Core Components:**
The project is a client daemon (`loopd`) that connects to a user's `lnd` node and communicates with a remote Loop server via gRPC. It is composed of several distinct managers and services that handle different aspects of the swap lifecycle.

*   **`loopd` (Daemon):** The main executable that bootstraps and orchestrates all other components. It initializes the database, connects to `lnd`, starts the gRPC/REST servers, and launches all the feature-specific managers (`liquidity`, `instantout`, `staticaddr`, etc.). Its lifecycle and configuration are managed in the `loopd/` and `cmd/loopd/` packages.

*   **Finite State Machines (FSM):** The core logic for managing swaps is built around a robust FSM pattern (`fsm/`). Each swap type (Instant Out, Static Address Loop-in, etc.) has its own state machine that defines its lifecycle. This ensures that swaps progress through a defined set of states, with transitions triggered by on-chain or off-chain events, making the system resilient to failures and restarts.

*   **Database (`loopdb/`):** Persistence is handled by a database layer that supports both `sqlite3` and `PostgreSQL` (via `sqlc` for query generation). There is also deprecated `bbolt` implementation which is left in the codebase to support migrating old instances. It stores swap contracts, state history, liquidity parameters, and other critical data, allowing the daemon to recover and resume operations after a restart.

*   **Notification Manager (`notifications/`):** Manages a persistent gRPC subscription to the Loop server. It listens for asynchronous events, such as a `Reservation` being offered or a request to co-sign a `StaticAddress` sweep, and dispatches them to the appropriate internal manager.

*   **On-Chain Transaction Management:**
    *   **Sweeper (`sweep/`):** A utility library responsible for crafting and signing sweep transactions for various HTLC types (P2WSH, P2TR, etc.).
    *   **Sweep Batcher (`sweepbatcher/`):** A sophisticated service that batches multiple on-chain sweeps into a single transaction to save on-chain fees. It has its own database store and logic for creating, tracking, and RBF-bumping (Replace-By-Fee) batch transactions until they are confirmed.

**3. Swap Types & Features:**

*   #### Standard Loop Out
    *   **Goal:** Move funds from Lightning to an on-chain address.
    *   **Mechanism:** The client pays an off-chain Lightning invoice to the Loop server. In return, the server sends an equivalent amount (minus fees) to the user's on-chain address via an on-chain HTLC. The client sweeps this HTLC using the same preimage from the Lightning payment, completing the atomic swap.

*   #### Standard Loop In
    *   **Goal:** Move on-chain funds into the Lightning Network.
    *   **Mechanism:** The client sends funds to an on-chain HTLC. This HTLC is spendable by the server *if* it knows the swap preimage (which it learns by paying the client's off-chain Lightning invoice), or by the client after a timeout. Once the server sees the on-chain HTLC, it pays the invoice to "loop in" the funds and then sweeps the HTLC with the revealed preimage, often in cooperation with the client to save fees.

*   #### Instant Loop-Out (`instantout/`)
    *   **Goal:** A faster, more efficient Loop-Out that prioritizes a cooperative path.
    *   **Mechanism:** Built on **Reservations**—on-chain UTXOs controlled by a 2-of-2 MuSig2 key between the client and server. The primary flow is a cooperative "Sweepless Sweep" that spends the reservation directly to the client's destination. A non-cooperative fallback to a standard HTLC ensures the swap remains non-custodial.

*   #### Static Address (`staticaddr/`)
    *   **Goal:** Provide a permanent, reusable on-chain address for receiving funds that can then be looped into Lightning.
    *   **Mechanism:** The address is a P2TR (Taproot) output with a cooperative MuSig2 keyspend path and a unilateral CSV timeout scriptspend path for safety. This feature is composed of several sub-managers:
        *   `address/`: Manages the creation and lifecycle of the static address itself.
        *   `deposit/`: Monitors the address for new UTXOs (deposits) and manages their state (e.g., `Deposited`, `Expired`, `LoopingIn`).
        *   `loopin/`: Orchestrates the Loop-In process for funds held at the static address.
        *   `withdraw/`: Handles cooperative withdrawal of funds from the address back to the user's wallet without performing a swap.

*   #### Liquidity Manager (`liquidity/`)
    *   **Goal:** Automate channel liquidity management based on user-defined rules.
    *   **Mechanism:** Periodically assesses channel balances and can suggest or automatically dispatch swaps to meet target liquidity levels. It supports detailed rules, fee budgets, and an "easyAutoloop" mode for simple balance targeting.

**4. Technology & Cross-Cutting Concerns:**

*   **Language:** Go
*   **Remote Communication:** gRPC. Definitions are split between `looprpc/` (client-facing) and `swapserverrpc/` (server-facing).
*   **Cryptography:** Extensively uses Taproot and MuSig2 for efficiency, privacy, and complex spending conditions, especially in the `instantout` and `staticaddr` features.
*   **Labeling (`labels/`):** A utility to create and validate labels for swaps, which helps distinguish between user-initiated and automated swaps (e.g., `[reserved]: autoloop-out`).
*   **Assets (`assets/`):** Contains logic for interacting with `tapd` (Taproot Assets Protocol Daemon), allowing Loop to facilitate swaps involving assets other than Bitcoin.
