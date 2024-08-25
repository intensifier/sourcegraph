import React, { useCallback, useMemo, useState, type PropsWithChildren } from 'react'

import { VisuallyHidden } from '@reach/visually-hidden'
import classNames from 'classnames'

import { isErrorLike, type ErrorLike } from '@sourcegraph/common'
import {
    SearchContextsOrderBy,
    type ListSearchContextsResult,
    type ListSearchContextsVariables,
    type SearchContextMinimalFields,
} from '@sourcegraph/shared/src/graphql-operations'
import type { PlatformContextProps } from '@sourcegraph/shared/src/platform/context'
import type { SearchContextProps } from '@sourcegraph/shared/src/search'
import { ErrorAlert, LoadingSpinner } from '@sourcegraph/wildcard'

import type { AuthenticatedUser } from '../../auth'
import { FilteredConnection, type Connection, type Filter } from '../../components/FilteredConnection'
import { useAffiliatedNamespaces } from '../../namespaces/useAffiliatedNamespaces'

import { useDefaultContext } from './hooks/useDefaultContext'
import { SearchContextNode, type SearchContextNodeProps } from './SearchContextNode'

import styles from './SearchContextsList.module.scss'

export interface SearchContextsListProps
    extends Pick<SearchContextProps, 'fetchSearchContexts'>,
        PlatformContextProps<'requestGraphQL'> {
    authenticatedUser: AuthenticatedUser | null
    setAlert: (message: string) => void
}

const GLOBAL_NAMESPACE_KEY = 'global' as const

export const SearchContextsList: React.FunctionComponent<SearchContextsListProps> = ({
    authenticatedUser,
    fetchSearchContexts,
    platformContext,
    setAlert,
}) => {
    const { namespaces, loading: namespacesLoading, error: namespacesError } = useAffiliatedNamespaces()

    const queryConnection = useCallback(
        (args: Omit<Partial<ListSearchContextsVariables>, 'first'> & { first?: number | null }) => {
            const { namespace, orderBy, descending } = args as {
                namespace: string | undefined
                orderBy: SearchContextsOrderBy
                descending: boolean
            }
            return fetchSearchContexts({
                first: args.first ?? 10,
                query: args.query ?? undefined,
                after: args.after ?? undefined,
                namespaces: namespace
                    ? [namespace === GLOBAL_NAMESPACE_KEY ? null : namespace]
                    : [null, ...(namespaces?.map(namespace => namespace.id) ?? [])],
                orderBy,
                descending,
                platformContext,
            })
        },
        [fetchSearchContexts, namespaces, platformContext]
    )

    const filters = useMemo<Filter[]>(
        () => [
            {
                label: 'Sort',
                type: 'select',
                id: 'order',
                tooltip: 'Order search contexts',
                options: [
                    {
                        value: 'spec-asc',
                        label: 'By name',
                        args: {
                            orderBy: SearchContextsOrderBy.SEARCH_CONTEXT_SPEC,
                            descending: false,
                        },
                    },
                    {
                        value: 'updated-at-desc',
                        label: 'Recently updated',
                        args: {
                            orderBy: SearchContextsOrderBy.SEARCH_CONTEXT_UPDATED_AT,
                            descending: true,
                        },
                    },
                ],
            },
            {
                label: 'Owner',
                type: 'select',
                id: 'owner',
                tooltip: 'Search context owner',
                options: [
                    {
                        value: 'all',
                        label: 'All',
                        args: {},
                    },
                    {
                        value: 'global-owner',
                        label: 'Global',
                        args: {
                            namespace: GLOBAL_NAMESPACE_KEY,
                        },
                    },
                    ...(namespaces?.map(namespace => ({
                        value: namespace.id,
                        label: namespace.namespaceName,
                        args: {
                            namespace: namespace.id,
                        },
                    })) ?? []),
                ],
            },
        ],
        [namespaces]
    )

    const [contextsOrError, setContextsOrError] = useState<
        Connection<SearchContextMinimalFields> | ErrorLike | undefined
    >()
    const onUpdateContexts = useCallback((contexts: Connection<SearchContextMinimalFields> | ErrorLike | undefined) => {
        setContextsOrError(contexts)
    }, [])

    const initialDefaultContext = useMemo(() => {
        if (!contextsOrError || isErrorLike(contextsOrError)) {
            return undefined
        }
        return contextsOrError.nodes.find(context => context.viewerHasAsDefault)
    }, [contextsOrError])

    const { defaultContext, setAsDefault } = useDefaultContext(initialDefaultContext?.id)
    const setAsDefaultWithErrorHandling = useCallback(
        (contextId: string, userId: string | undefined) => {
            // Clear previous error
            setAlert('')
            return setAsDefault(contextId, userId).catch(error => {
                if (isErrorLike(error)) {
                    setAlert(error.message)
                }
            })
        },
        [setAlert, setAsDefault]
    )

    const error = namespacesError
    const loading = namespacesLoading

    return loading ? (
        <LoadingSpinner />
    ) : error ? (
        <ErrorAlert error={error} className="mb-3" />
    ) : (
        <FilteredConnection<
            SearchContextMinimalFields,
            Omit<SearchContextNodeProps, 'node'>,
            ListSearchContextsResult['searchContexts']
        >
            listComponent="table"
            contentWrapperComponent={SearchContextsTableWrapper}
            headComponent={SearchContextsTableHeader}
            defaultFirst={10}
            compact={false}
            queryConnection={queryConnection}
            filters={filters}
            hideSearch={false}
            showSearchFirst={true}
            nodeComponent={SearchContextNode}
            nodeComponentProps={{
                authenticatedUser,
                setAlert,
                defaultContext,
                setAsDefault: setAsDefaultWithErrorHandling,
            }}
            noun="search context"
            pluralNoun="search contexts"
            cursorPaging={true}
            inputClassName={classNames(styles.filterInput)}
            inputPlaceholder="Find a context..."
            inputAriaLabel="Find a context..."
            formClassName={styles.filtersForm}
            onUpdate={onUpdateContexts}
        />
    )
}

const SearchContextsTableWrapper: React.FunctionComponent<PropsWithChildren<{}>> = ({ children }) => (
    <div className={styles.tableWrapper}>{children}</div>
)

const SearchContextsTableHeader: React.FunctionComponent = () => (
    <thead>
        <tr>
            <th>
                <VisuallyHidden>Starred</VisuallyHidden>
            </th>
            <th>Name</th>
            <th>Description</th>
            <th>Contents</th>
            <th>Last updated</th>
            <th>
                <VisuallyHidden>Tags</VisuallyHidden>
            </th>
            <th>
                <VisuallyHidden>Actions</VisuallyHidden>
            </th>
        </tr>
    </thead>
)
