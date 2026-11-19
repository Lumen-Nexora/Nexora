#![no_std]

use soroban_sdk::{contract, contracterror, contractimpl, contracttype, symbol_short, Address, Env, Vec, u64};

#[derive(Clone, Copy)]
#[contracttype]
pub enum CircuitState {
    Closed = 0,
    Open = 1,
    HalfOpen = 2,
}

#[derive(Clone)]
#[contracttype]
pub enum DataKey {
    Admin,
    Implementation,
    CircuitState,
    FailureCount,
    LastFailureTime,
    FailureThreshold,
    RecoveryTimeout,
    PreviousImplementations,
}

#[derive(Clone)]
#[contracttype]
pub struct CircuitBreakerState {
    pub state: CircuitState,
    pub failure_count: u32,
    pub last_failure_time: u64,
}

#[contracterror]
#[derive(Copy, Clone, Debug, Eq, PartialEq, PartialOrd, Ord)]
#[repr(u32)]
pub enum CircuitError {
    NotInitialized = 1,
    AlreadyInitialized = 2,
    InvalidAdmin = 3,
    CircuitOpen = 4,
    UpgradeFailed = 5,
    InvalidImplementation = 6,
    RecoveryInProgress = 7,
}

#[contract]
pub struct CircuitBreakerProxy;

#[contractimpl]
impl CircuitBreakerProxy {
    pub fn initialize(
        env: Env,
        admin: Address,
        implementation: Address,
        failure_threshold: u32,
        recovery_timeout_secs: u64,
    ) -> Result<(), CircuitError> {
        if env.storage().persistent().has(&DataKey::Admin) {
            return Err(CircuitError::AlreadyInitialized);
        }

        admin.require_auth();

        env.storage().persistent().set(&DataKey::Admin, &admin);
        env.storage().persistent().set(&DataKey::Implementation, &implementation);
        env.storage().persistent().set(
            &DataKey::CircuitState,
            &CircuitBreakerState {
                state: CircuitState::Closed,
                failure_count: 0,
                last_failure_time: 0,
            },
        );
        env.storage()
            .persistent()
            .set(&DataKey::FailureThreshold, &failure_threshold);
        env.storage()
            .persistent()
            .set(&DataKey::RecoveryTimeout, &recovery_timeout_secs);
        env.storage()
            .persistent()
            .set(&DataKey::PreviousImplementations, &Vec::<Address>::new(&env));

        Ok(())
    }

    pub fn upgrade_implementation(
        env: Env,
        new_implementation: Address,
    ) -> Result<(), CircuitError> {
        let admin: Address = env
            .storage()
            .persistent()
            .get(&DataKey::Admin)
            .ok_or(CircuitError::NotInitialized)?;

        admin.require_auth();

        let current_impl: Address = env
            .storage()
            .persistent()
            .get(&DataKey::Implementation)
            .ok_or(CircuitError::NotInitialized)?;

        let mut previous: Vec<Address> = env
            .storage()
            .persistent()
            .get(&DataKey::PreviousImplementations)
            .unwrap_or_else(|| Vec::new(&env));

        previous.push_back(current_impl);
        env.storage()
            .persistent()
            .set(&DataKey::PreviousImplementations, &previous);

        env.storage()
            .persistent()
            .set(&DataKey::Implementation, &new_implementation);

        Ok(())
    }

    pub fn record_failure(env: Env) -> Result<(), CircuitError> {
        let mut state: CircuitBreakerState = env
            .storage()
            .persistent()
            .get(&DataKey::CircuitState)
            .ok_or(CircuitError::NotInitialized)?;

        let threshold: u32 = env
            .storage()
            .persistent()
            .get(&DataKey::FailureThreshold)
            .unwrap_or(5);

        state.failure_count = state.failure_count.saturating_add(1);
        state.last_failure_time = env.ledger().timestamp();

        if state.failure_count >= threshold {
            state.state = CircuitState::Open;
        }

        env.storage()
            .persistent()
            .set(&DataKey::CircuitState, &state);

        Ok(())
    }

    pub fn record_success(env: Env) -> Result<(), CircuitError> {
        let mut state: CircuitBreakerState = env
            .storage()
            .persistent()
            .get(&DataKey::CircuitState)
            .ok_or(CircuitError::NotInitialized)?;

        match state.state {
            CircuitState::Closed => {
                state.failure_count = 0;
            }
            CircuitState::HalfOpen => {
                state.state = CircuitState::Closed;
                state.failure_count = 0;
            }
            CircuitState::Open => {}
        }

        env.storage()
            .persistent()
            .set(&DataKey::CircuitState, &state);

        Ok(())
    }

    pub fn attempt_recovery(env: Env) -> Result<(), CircuitError> {
        let mut state: CircuitBreakerState = env
            .storage()
            .persistent()
            .get(&DataKey::CircuitState)
            .ok_or(CircuitError::NotInitialized)?;

        if state.state != CircuitState::Open {
            return Err(CircuitError::RecoveryInProgress);
        }

        let recovery_timeout: u64 = env
            .storage()
            .persistent()
            .get(&DataKey::RecoveryTimeout)
            .unwrap_or(300);

        let current_time = env.ledger().timestamp();
        if current_time.saturating_sub(state.last_failure_time) < recovery_timeout {
            return Err(CircuitError::CircuitOpen);
        }

        state.state = CircuitState::HalfOpen;
        state.failure_count = 0;

        env.storage()
            .persistent()
            .set(&DataKey::CircuitState, &state);

        Ok(())
    }

    pub fn get_circuit_state(env: Env) -> Result<CircuitBreakerState, CircuitError> {
        env.storage()
            .persistent()
            .get(&DataKey::CircuitState)
            .ok_or(CircuitError::NotInitialized)
    }

    pub fn get_implementation(env: Env) -> Result<Address, CircuitError> {
        env.storage()
            .persistent()
            .get(&DataKey::Implementation)
            .ok_or(CircuitError::NotInitialized)
    }

    pub fn get_previous_implementations(env: Env) -> Result<Vec<Address>, CircuitError> {
        env.storage()
            .persistent()
            .get(&DataKey::PreviousImplementations)
            .ok_or(CircuitError::NotInitialized)
    }

    pub fn is_circuit_open(env: Env) -> Result<bool, CircuitError> {
        let state: CircuitBreakerState = env
            .storage()
            .persistent()
            .get(&DataKey::CircuitState)
            .ok_or(CircuitError::NotInitialized)?;

        Ok(matches!(state.state, CircuitState::Open))
    }

    pub fn reset_circuit(env: Env) -> Result<(), CircuitError> {
        let admin: Address = env
            .storage()
            .persistent()
            .get(&DataKey::Admin)
            .ok_or(CircuitError::NotInitialized)?;

        admin.require_auth();

        let mut state: CircuitBreakerState = env
            .storage()
            .persistent()
            .get(&DataKey::CircuitState)
            .ok_or(CircuitError::NotInitialized)?;

        state.state = CircuitState::Closed;
        state.failure_count = 0;
        state.last_failure_time = 0;

        env.storage()
            .persistent()
            .set(&DataKey::CircuitState, &state);

        Ok(())
    }
}

#[cfg(test)]
mod tests {
    use super::*;
    use soroban_sdk::testutils::{Address as _, Env as _, Ledger as _};

    #[test]
    fn test_initialize() {
        let env = Env::default();
        let admin = Address::random(&env);
        let impl_addr = Address::random(&env);

        let result = CircuitBreakerProxy::initialize(env, admin, impl_addr, 5, 300);
        assert!(result.is_ok());
    }

    #[test]
    fn test_circuit_state() {
        let env = Env::default();
        let admin = Address::random(&env);
        let impl_addr = Address::random(&env);

        CircuitBreakerProxy::initialize(env.clone(), admin, impl_addr, 5, 300).unwrap();

        let state = CircuitBreakerProxy::get_circuit_state(env).unwrap();
        assert!(matches!(state.state, CircuitState::Closed));
    }

    #[test]
    fn test_record_failure_trip_circuit() {
        let env = Env::default();
        let admin = Address::random(&env);
        let impl_addr = Address::random(&env);

        CircuitBreakerProxy::initialize(env.clone(), admin, impl_addr, 3, 300).unwrap();

        assert!(!CircuitBreakerProxy::is_circuit_open(env.clone()).unwrap());

        CircuitBreakerProxy::record_failure(env.clone()).unwrap();
        CircuitBreakerProxy::record_failure(env.clone()).unwrap();
        assert!(!CircuitBreakerProxy::is_circuit_open(env.clone()).unwrap());

        // Third failure hits threshold (3) -> opens circuit
        CircuitBreakerProxy::record_failure(env.clone()).unwrap();
        let state = CircuitBreakerProxy::get_circuit_state(env.clone()).unwrap();
        assert!(matches!(state.state, CircuitState::Open));
        assert_eq!(state.failure_count, 3);
        assert!(CircuitBreakerProxy::is_circuit_open(env.clone()).unwrap());
    }

    #[test]
    fn test_record_success_resets_failures() {
        let env = Env::default();
        let admin = Address::random(&env);
        let impl_addr = Address::random(&env);

        CircuitBreakerProxy::initialize(env.clone(), admin, impl_addr, 5, 300).unwrap();

        CircuitBreakerProxy::record_failure(env.clone()).unwrap();
        CircuitBreakerProxy::record_failure(env.clone()).unwrap();
        let state_before = CircuitBreakerProxy::get_circuit_state(env.clone()).unwrap();
        assert_eq!(state_before.failure_count, 2);

        CircuitBreakerProxy::record_success(env.clone()).unwrap();
        let state_after = CircuitBreakerProxy::get_circuit_state(env.clone()).unwrap();
        assert_eq!(state_after.failure_count, 0);
        assert!(matches!(state_after.state, CircuitState::Closed));
    }

    #[test]
    fn test_recovery_timeout_lifecycle() {
        let env = Env::default();
        let admin = Address::random(&env);
        let impl_addr = Address::random(&env);

        env.ledger().set_timestamp(1000);
        CircuitBreakerProxy::initialize(env.clone(), admin.clone(), impl_addr, 2, 300).unwrap();

        // Trip circuit
        CircuitBreakerProxy::record_failure(env.clone()).unwrap();
        CircuitBreakerProxy::record_failure(env.clone()).unwrap();
        assert!(CircuitBreakerProxy::is_circuit_open(env.clone()).unwrap());

        // Attempt recovery before timeout (elapsed 100s < 300s timeout) -> rejected
        env.ledger().set_timestamp(1100);
        let early_res = CircuitBreakerProxy::attempt_recovery(env.clone());
        assert_eq!(early_res, Err(CircuitError::CircuitOpen));

        // Advance timestamp past timeout (elapsed 350s > 300s) -> transitions to HalfOpen
        env.ledger().set_timestamp(1350);
        let recovery_res = CircuitBreakerProxy::attempt_recovery(env.clone());
        assert!(recovery_res.is_ok());

        let state = CircuitBreakerProxy::get_circuit_state(env.clone()).unwrap();
        assert!(matches!(state.state, CircuitState::HalfOpen));
        assert_eq!(state.failure_count, 0);

        // Success while HalfOpen closes circuit
        CircuitBreakerProxy::record_success(env.clone()).unwrap();
        let final_state = CircuitBreakerProxy::get_circuit_state(env.clone()).unwrap();
        assert!(matches!(final_state.state, CircuitState::Closed));
    }

    #[test]
    fn test_admin_manual_reset() {
        let env = Env::default();
        let admin = Address::random(&env);
        let impl_addr = Address::random(&env);

        CircuitBreakerProxy::initialize(env.clone(), admin.clone(), impl_addr, 1, 300).unwrap();

        // Trip circuit
        CircuitBreakerProxy::record_failure(env.clone()).unwrap();
        assert!(CircuitBreakerProxy::is_circuit_open(env.clone()).unwrap());

        // Admin resets circuit manually
        CircuitBreakerProxy::reset_circuit(env.clone()).unwrap();

        let state = CircuitBreakerProxy::get_circuit_state(env.clone()).unwrap();
        assert!(matches!(state.state, CircuitState::Closed));
        assert_eq!(state.failure_count, 0);
    }

    #[test]
    fn test_upgrade_implementation_and_history() {
        let env = Env::default();
        let admin = Address::random(&env);
        let impl1 = Address::random(&env);
        let impl2 = Address::random(&env);

        CircuitBreakerProxy::initialize(env.clone(), admin.clone(), impl1.clone(), 5, 300).unwrap();

        assert_eq!(CircuitBreakerProxy::get_implementation(env.clone()).unwrap(), impl1);
        let prev_initial = CircuitBreakerProxy::get_previous_implementations(env.clone()).unwrap();
        assert_eq!(prev_initial.len(), 0);

        // Upgrade implementation
        CircuitBreakerProxy::upgrade_implementation(env.clone(), impl2.clone()).unwrap();
        assert_eq!(CircuitBreakerProxy::get_implementation(env.clone()).unwrap(), impl2);

        let prev_after = CircuitBreakerProxy::get_previous_implementations(env.clone()).unwrap();
        assert_eq!(prev_after.len(), 1);
        assert_eq!(prev_after.get(0).unwrap(), impl1);
    }

    #[test]
    fn test_prevent_duplicate_initialization() {
        let env = Env::default();
        let admin = Address::random(&env);
        let impl_addr = Address::random(&env);

        CircuitBreakerProxy::initialize(env.clone(), admin.clone(), impl_addr.clone(), 5, 300).unwrap();
        let dup = CircuitBreakerProxy::initialize(env.clone(), admin, impl_addr, 5, 300);
        assert_eq!(dup, Err(CircuitError::AlreadyInitialized));
    }

    #[test]
    fn test_uninitialized_calls_rejected() {
        let env = Env::default();
        assert_eq!(CircuitBreakerProxy::record_failure(env.clone()), Err(CircuitError::NotInitialized));
        assert_eq!(CircuitBreakerProxy::get_circuit_state(env.clone()), Err(CircuitError::NotInitialized));
    }
}
