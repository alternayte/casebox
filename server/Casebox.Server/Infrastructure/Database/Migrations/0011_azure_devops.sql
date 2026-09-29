-- Azure DevOps Server (docs/specs/azure-devops.md): a second code host, and the host of each
-- workspace repository, stored with the repository when it is added.
ALTER TABLE casebox.integrations DROP CONSTRAINT integrations_kind_check;
ALTER TABLE casebox.integrations ADD CONSTRAINT integrations_kind_check CHECK (kind IN ('github', 'jira', 'azure-devops'));

-- Projection of workspace.repo_added: which code host serves a repository, and for Azure DevOps
-- the collection URL its API and clone URL start with.
CREATE TABLE casebox.repositories (
    org_id     text        NOT NULL,
    repo       text        NOT NULL,
    host       text        NOT NULL CHECK (host IN ('github', 'azure-devops')),
    collection text,
    updated_at timestamptz NOT NULL,
    PRIMARY KEY (org_id, repo)
);

-- Every repository added before this step is a GitHub repository.
INSERT INTO casebox.repositories (org_id, repo, host, updated_at)
SELECT DISTINCT org_id, jsonb_array_elements_text(repos), 'github', now() FROM casebox.workspaces
ON CONFLICT DO NOTHING;
