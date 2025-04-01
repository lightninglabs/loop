-- We kept old table as sweeps_old. Use it.
ALTER TABLE sweeps RENAME TO sweeps_new;
ALTER TABLE sweeps_old RENAME TO sweeps;
