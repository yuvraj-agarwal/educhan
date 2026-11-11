-- Attachments kept the filename the poster's browser sent with an image, and
-- showed it under the image. A camera's or a phone's filename names a date and
-- sometimes a person (IMG_20261013_Priya.jpg), so every image is now shown as
-- "image" and the extension of the format it was stored in, which the server
-- chose (attachmentName in attachments.go). This gives the rows from before the
-- same name, from the extension of the stored path, so no filename a poster
-- sent is left in the table.
--
-- The column keeps its name, and holds only that from now on. Rewriting a row
-- does not scrub its old version from the database's files at once, nor from
-- any backup taken before this ran.
--
-- Nor does it touch the files. An image stored before this release was kept
-- as it was uploaded, metadata and all, and is served that way until its post
-- is deleted; this migration's applied_at in schema_migrations is the cutoff
-- the README's query for them uses.
UPDATE attachments
SET original_name = 'image' || COALESCE(substring(original_path FROM '\.[^./]*$'), '');
