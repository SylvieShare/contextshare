CREATE TABLE IF NOT EXISTS cs.users (
    id text PRIMARY KEY,
    email text NOT NULL UNIQUE,
    password_hash bytea NOT NULL,
    created_at timestamptz NOT NULL DEFAULT now()
);
CREATE TABLE IF NOT EXISTS cs.sessions (
    token_hash bytea PRIMARY KEY,
    user_id text NOT NULL REFERENCES cs.users(id) ON DELETE CASCADE,
    expires_at timestamptz NOT NULL
);
CREATE TABLE IF NOT EXISTS cs.projects (
    id text PRIMARY KEY,
    name text NOT NULL,
    overview text NOT NULL DEFAULT '',
    repository_url text NOT NULL DEFAULT '',
    updated_at timestamptz NOT NULL DEFAULT now()
);
CREATE TABLE IF NOT EXISTS cs.memberships (
    project_id text NOT NULL REFERENCES cs.projects(id) ON DELETE CASCADE,
    user_id text NOT NULL REFERENCES cs.users(id) ON DELETE CASCADE,
    role text NOT NULL CHECK (role IN ('owner','editor','viewer')),
    PRIMARY KEY (project_id,user_id)
);
CREATE TABLE IF NOT EXISTS cs.invites (
    token_hash bytea PRIMARY KEY,
    project_id text REFERENCES cs.projects(id) ON DELETE CASCADE,
    role text CHECK (role IN ('editor','viewer')),
    expires_at timestamptz NOT NULL,
    used_by text REFERENCES cs.users(id),
    CHECK ((project_id IS NULL) = (role IS NULL))
);
CREATE TABLE IF NOT EXISTS cs.sections (
    id text PRIMARY KEY,
    project_id text NOT NULL REFERENCES cs.projects(id) ON DELETE CASCADE,
    title text NOT NULL,
    summary text NOT NULL DEFAULT '',
    position int NOT NULL DEFAULT 0,
    UNIQUE(project_id,id)
);
CREATE TABLE IF NOT EXISTS cs.nodes (
    id text PRIMARY KEY,
    project_id text NOT NULL,
    section_id text NOT NULL,
    parent_id text,
    title text NOT NULL,
    kind text NOT NULL CHECK (kind IN ('feature','rule','decision','limitation','guide','evidence')),
    body text NOT NULL,
    status text NOT NULL CHECK (status IN ('current','planned','historical')),
    source_url text NOT NULL DEFAULT '',
    source_revision text NOT NULL DEFAULT '',
    verified_at date,
    version bigint NOT NULL DEFAULT 1,
    updated_at timestamptz NOT NULL DEFAULT now(),
    UNIQUE(project_id,id),
    UNIQUE(project_id,section_id,id),
    CHECK (parent_id IS NULL OR parent_id <> id),
    FOREIGN KEY (project_id,section_id) REFERENCES cs.sections(project_id,id) ON DELETE CASCADE,
    FOREIGN KEY (project_id,section_id,parent_id) REFERENCES cs.nodes(project_id,section_id,id)
);
CREATE TABLE IF NOT EXISTS cs.node_platforms (
    project_id text NOT NULL,
    node_id text NOT NULL,
    platform text NOT NULL CHECK (platform IN ('web','android','backend')),
    implementation text NOT NULL,
    verification text NOT NULL DEFAULT '',
    availability text NOT NULL DEFAULT '',
    PRIMARY KEY (project_id,node_id,platform),
    FOREIGN KEY (project_id,node_id) REFERENCES cs.nodes(project_id,id) ON DELETE CASCADE
);
CREATE TABLE IF NOT EXISTS cs.node_links (
    project_id text NOT NULL,
    from_id text NOT NULL,
    to_id text NOT NULL,
    kind text NOT NULL CHECK (kind IN ('related','depends_on','verified_by','supersedes')),
    CHECK (from_id <> to_id),
    PRIMARY KEY (project_id,from_id,to_id,kind),
    FOREIGN KEY (project_id,from_id) REFERENCES cs.nodes(project_id,id) ON DELETE CASCADE,
    FOREIGN KEY (project_id,to_id) REFERENCES cs.nodes(project_id,id) ON DELETE CASCADE
);
CREATE TABLE IF NOT EXISTS cs.requests (
    id text PRIMARY KEY,
    project_id text NOT NULL REFERENCES cs.projects(id) ON DELETE CASCADE,
    section_id text,
    node_id text,
    author_id text NOT NULL REFERENCES cs.users(id),
    title text NOT NULL,
    body text NOT NULL,
    kind text NOT NULL CHECK (kind IN ('feature','bug','idea')),
    state text NOT NULL DEFAULT 'inbox' CHECK (state IN ('inbox','ready','active','done','archived')),
    criteria text NOT NULL DEFAULT '',
    version bigint NOT NULL DEFAULT 1,
    created_at timestamptz NOT NULL DEFAULT now(),
    updated_at timestamptz NOT NULL DEFAULT now(),
    UNIQUE(project_id,id),
    FOREIGN KEY (project_id,section_id) REFERENCES cs.sections(project_id,id),
    FOREIGN KEY (project_id,node_id) REFERENCES cs.nodes(project_id,id)
);
CREATE TABLE IF NOT EXISTS cs.request_work (
    project_id text NOT NULL,
    request_id text NOT NULL,
    platform text NOT NULL CHECK (platform IN ('web','android','backend')),
    state text NOT NULL CHECK (state IN ('ready','active','done')),
    assignee_id text REFERENCES cs.users(id),
    evidence text NOT NULL DEFAULT '',
    PRIMARY KEY(project_id,request_id,platform),
    FOREIGN KEY(project_id,request_id) REFERENCES cs.requests(project_id,id) ON DELETE CASCADE
);
CREATE TABLE IF NOT EXISTS cs.request_batches (
    project_id text NOT NULL REFERENCES cs.projects(id) ON DELETE CASCADE,
    user_id text NOT NULL REFERENCES cs.users(id),
    operation_id text NOT NULL,
    input_hash bytea NOT NULL,
    response jsonb NOT NULL,
    PRIMARY KEY(project_id,user_id,operation_id)
);
CREATE TABLE IF NOT EXISTS cs.audit (
    id bigint GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
    project_id text NOT NULL REFERENCES cs.projects(id) ON DELETE CASCADE,
    user_id text NOT NULL REFERENCES cs.users(id),
    action text NOT NULL,
    entity_id text NOT NULL,
    at timestamptz NOT NULL DEFAULT now()
);
CREATE TABLE IF NOT EXISTS cs.grants (
    id text PRIMARY KEY,
    user_id text NOT NULL REFERENCES cs.users(id) ON DELETE CASCADE,
    project_id text NOT NULL REFERENCES cs.projects(id) ON DELETE CASCADE,
    label text NOT NULL,
    client_id text NOT NULL DEFAULT '',
    scopes text[] NOT NULL,
    expires_at timestamptz NOT NULL,
    revoked_at timestamptz
);
CREATE TABLE IF NOT EXISTS cs.tokens (
    hash bytea PRIMARY KEY,
    grant_id text NOT NULL REFERENCES cs.grants(id) ON DELETE CASCADE,
    kind text NOT NULL CHECK (kind IN ('access','refresh')),
    expires_at timestamptz NOT NULL
);
CREATE TABLE IF NOT EXISTS cs.oauth_clients (
    id text PRIMARY KEY,
    redirects text[] NOT NULL UNIQUE
);
CREATE TABLE IF NOT EXISTS cs.oauth_codes (
    hash bytea PRIMARY KEY,
    grant_id text NOT NULL REFERENCES cs.grants(id) ON DELETE CASCADE,
    redirect_uri text NOT NULL,
    challenge text NOT NULL,
    expires_at timestamptz NOT NULL
);
CREATE INDEX IF NOT EXISTS cs_sections_project ON cs.sections(project_id,position,id);
CREATE INDEX IF NOT EXISTS cs_nodes_project_section ON cs.nodes(project_id,section_id,status,id);
CREATE INDEX IF NOT EXISTS cs_links_reverse ON cs.node_links(project_id,to_id);
CREATE INDEX IF NOT EXISTS cs_requests_project_state ON cs.requests(project_id,state,created_at DESC);
CREATE INDEX IF NOT EXISTS cs_tokens_grant ON cs.tokens(grant_id);
