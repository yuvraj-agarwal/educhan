-- What PostgreSQL's statistics keep about the verification tables.
--
-- ANALYZE, which autovacuum runs by itself once enough rows have changed,
-- copies a sample of each column's values into pg_statistic: the most common
-- values and a histogram, readable through pg_stats by any role that can read
-- the table, and kept in a backup of the instance. For verification_pending
-- that is a sample of the email hashes of codes in flight, beside expiries to
-- the microsecond, and an email hash leads straight to the credential it
-- became. Neither the nightly rewrite nor a VACUUM FULL of either table
-- touches pg_statistic, and nothing educhan does needs these numbers: every
-- query on either table finds its rows by a unique key or by expiry. So no
-- column of either table is sampled any more, and the samples already taken
-- are deleted from pg_statistic: changing a column's type deletes its
-- statistics, and changing it to the type it already has changes nothing else.
--
-- Deleted, not erased. Like any deleted row, a sample stays in pg_statistic's
-- file — and one big enough to be stored out of line, as the email hashes'
-- histogram is, in the file of its TOAST table — where a superuser or root on
-- the machine can read it, and in every backup that copies those files, until
-- VACUUM FULL writes the catalog out anew; a plain vacuum, autovacuum's
-- included, leaves the bytes. The archive sweeper does that every night, after
-- the verification tables (RewriteCredentials), so a database upgraded across
-- this migration holds them until its first sweep. One that runs this
-- migration before any ANALYZE of the tables, as a new one does, has none.
--
-- Autovacuum is off on both tables. The times it last vacuumed and analysed a
-- table, which pg_stat_user_tables shows, follow the table's writes — a run
-- starts within a minute or so of the write that took the table past its
-- threshold — so they date a request or a confirmation. The archive
-- sweeper's nightly VACUUM FULL of both tables does what autovacuum would.
-- With autovacuum off PostgreSQL still vacuums a table whose transaction ids
-- are close to wrapping around, at a time set by how old its rows are rather
-- than by when anybody wrote, and the nightly VACUUM FULL keeps that from
-- coming due. A time recorded before this migration stays until the server's
-- statistics are reset, which this role cannot do.

ALTER TABLE verification_pending
    ALTER COLUMN email_hash TYPE bytea,
    ALTER COLUMN token_hash TYPE bytea,
    ALTER COLUMN code_hash TYPE bytea,
    ALTER COLUMN label TYPE text,
    ALTER COLUMN attempts TYPE integer,
    ALTER COLUMN expires_at TYPE timestamptz,
    ALTER COLUMN email_hash SET STATISTICS 0,
    ALTER COLUMN token_hash SET STATISTICS 0,
    ALTER COLUMN code_hash SET STATISTICS 0,
    ALTER COLUMN label SET STATISTICS 0,
    ALTER COLUMN attempts SET STATISTICS 0,
    ALTER COLUMN expires_at SET STATISTICS 0,
    SET (autovacuum_enabled = false);

ALTER TABLE verification_credentials
    ALTER COLUMN email_hash TYPE bytea,
    ALTER COLUMN credential_id_hash TYPE bytea,
    ALTER COLUMN label TYPE text,
    ALTER COLUMN expires_at TYPE timestamptz,
    ALTER COLUMN email_hash SET STATISTICS 0,
    ALTER COLUMN credential_id_hash SET STATISTICS 0,
    ALTER COLUMN label SET STATISTICS 0,
    ALTER COLUMN expires_at SET STATISTICS 0,
    SET (autovacuum_enabled = false);
