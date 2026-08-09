package linear

const SymphonyProjectScope = `query SymphonyProjectScope(
  $projectSlug: String!
  $first: Int!
) {
  projects(filter: { slugId: { eq: $projectSlug } }, first: $first) {
    nodes { id slugId }
    pageInfo { hasNextPage }
  }
}`

const SymphonyIssuesByStates = `query SymphonyIssuesByStates(
  $projectSlug: String!
  $stateNames: [String!]!
  $first: Int!
  $relationFirst: Int!
  $after: String
) {
  issues(
    filter: {
      project: { slugId: { eq: $projectSlug } }
      state: { name: { in: $stateNames } }
    }
    first: $first
    after: $after
    orderBy: createdAt
  ) {
    nodes {
      id
      identifier
      title
      description
      priority
      state { name }
      branchName
      url
      assignee { id }
      labels(first: 50) {
        nodes { name }
        pageInfo { hasNextPage }
      }
      inverseRelations(first: $relationFirst) {
        nodes {
          type
          issue {
            id
            identifier
            state { name }
          }
        }
        pageInfo { hasNextPage }
      }
      project { id slugId }
      team { id }
      createdAt
      updatedAt
    }
    pageInfo { hasNextPage endCursor }
  }
}`

const SymphonyIssuesByIDs = `query SymphonyIssuesByIDs(
  $ids: [ID!]!
  $projectSlug: String!
  $first: Int!
  $relationFirst: Int!
) {
  issues(
    filter: {
      id: { in: $ids }
      project: { slugId: { eq: $projectSlug } }
    }
    first: $first
  ) {
    nodes {
      id
      identifier
      title
      description
      priority
      state { name }
      branchName
      url
      assignee { id }
      labels(first: 50) {
        nodes { name }
        pageInfo { hasNextPage }
      }
      inverseRelations(first: $relationFirst) {
        nodes {
          type
          issue {
            id
            identifier
            state { name }
          }
        }
        pageInfo { hasNextPage }
      }
      project { id slugId }
      team { id }
      createdAt
      updatedAt
    }
    pageInfo { hasNextPage endCursor }
  }
}`
