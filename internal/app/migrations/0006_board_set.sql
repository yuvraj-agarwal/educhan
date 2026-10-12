-- The board set educhan opens with: subject boards, then student-life boards.
--
-- The three boards 0001 seeded were named for one campus. They are renamed
-- rather than replaced, so any threads on them stay put under the new address:
-- placements becomes career, research becomes grad, and general becomes meta,
-- which is about educhan itself rather than a catch-all; a general board tends
-- to take over a site's traffic.
--
-- Every statement is safe on any database. A rename happens only while its new
-- slug is free, and every other board is inserted with ON CONFLICT DO NOTHING,
-- so a board a moderator already opened under one of these slugs keeps its own
-- name, description, and settings, and the old board beside it is left alone.
--
-- Stored attachment paths keep the slug the board had at upload time
-- ("general/2026/08/..."). That is harmless: each row names its own files, so a
-- renamed board's images still load.

-- Where a board appears in the listing. Without it the renamed boards, being
-- the oldest rows, would lead the list. NULL is a board opened over the API,
-- listed after the seeded ones in the order it was created.
ALTER TABLE boards ADD COLUMN IF NOT EXISTS sort_order INTEGER;

-- The whole set, written once and read by each statement below. Names are kept
-- short because they label the board navigation; each description is a single
-- sentence.
CREATE TEMPORARY TABLE board_set (
    slug        TEXT PRIMARY KEY,
    name        TEXT NOT NULL,
    description TEXT NOT NULL,
    sort_order  INTEGER NOT NULL
) ON COMMIT DROP;

INSERT INTO board_set (slug, name, description, sort_order) VALUES
    ('math',   'Mathematics',      'Mathematics at every level, from homework problems to proofs.',             10),
    ('phys',   'Physics',          'Physics at every level, from mechanics to quantum theory.',                 20),
    ('chem',   'Chemistry',        'Chemistry, from the periodic table to the lab bench.',                      30),
    ('bio',    'Biology',          'Biology, from cells and genetics to whole ecosystems.',                     40),
    ('cs',     'Computer Science', 'Programming, algorithms, and the theory of computing.',                     50),
    ('eng',    'Engineering',      'Engineering of every kind, from circuits to bridges.',                      60),
    ('med',    'Medicine',         'Studying medicine and the health sciences.',                                70),
    ('law',    'Law',              'Studying law: cases, statutes, and legal reasoning.',                       80),
    ('econ',   'Economics',        'Economics, from supply and demand to econometrics.',                        90),
    ('lit',    'Literature',       'Books, poetry, and plays, and how to read them closely.',                  100),
    ('his',    'History',          'History of every period and every part of the world.',                     110),
    ('phil',   'Philosophy',       'Philosophy, logic, and ethics.',                                           120),
    ('lang',   'Languages',        'Learning languages, and the study of language itself.',                    130),
    ('adm',    'Admissions',       'Admissions and entrance exams at every level: SAT, JEE, UCAS, gaokao and the like.', 140),
    ('exam',   'Exams',            'Revision, past papers, and getting through exam season.',                  150),
    ('grad',   'Grad School',      'Graduate study and research: applications, supervisors, and lab life.',    160),
    ('career', 'Careers',          'Internships, placements, interviews, and offers.',                         170),
    ('abroad', 'Study Abroad',     'Studying in another country: applications, visas, and settling in.',       180),
    ('adv',    'Advice',           'Advice on studying, school, and student life.',                            190),
    ('meta',   'Meta',             'About educhan itself: feedback, bugs, and requests for new boards.',       200);

-- The renames, which take the new name and description with the new slug. The
-- old and new slugs are disjoint, so one statement can move all three.
UPDATE boards b
SET slug = s.slug, name = s.name, description = s.description
FROM (VALUES ('placements', 'career'), ('research', 'grad'), ('general', 'meta')) AS r (old_slug, new_slug)
JOIN board_set s ON s.slug = r.new_slug
WHERE b.slug = r.old_slug
  AND NOT EXISTS (SELECT 1 FROM boards taken WHERE taken.slug = r.new_slug);

-- Everything else, and career, grad, or meta too on a database that never had
-- the board they are renamed from.
INSERT INTO boards (slug, name, description)
SELECT slug, name, description FROM board_set
ORDER BY sort_order
ON CONFLICT (slug) DO NOTHING;

-- The listing order belongs to the slug, so it applies to whichever row holds
-- one: seeded, renamed, or opened by a moderator before this ran.
UPDATE boards b SET sort_order = s.sort_order
FROM board_set s
WHERE b.slug = s.slug;
