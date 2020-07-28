# postdock

postdock allows you to run psql or pg_dump either inside a running docker container
or by pulling & running a lightweight postgres container such as postgres-11.8-alpine.

If run as a container, this package will invoke docker run with `--rm` and so the 
container exists when the command completes.

Some common commands one might run _before_ your database is created but after
you have spun up a postgres instance.

- Create: create a database 
- Exists: check if a database already exists
- Terminate: terminates an existing session
- Drop: drops a database
- Import: enables importing a database from a sql file (think schema file)
- SchemaDump: a `pg_dump` schema-only, cleaned up and outputted

Remember, when invoking this package _inside_ a docker container its assumed
`psql` and `pg_dump` are available. In most cases you would build an
image which will have all the tools your development team needs.

And `outside` a docker container, this package will use whatever image you specify.
This is just one example: `postgres-11.8-alpine`

## But why?

The ability to use a single package to create, drop, import, and dump database for 
all your needs.

For example, say you have 3 use cases.

1.  during local development all devs have a common way to create a databse and apply
    migrations. 

2.  CI step to test migrations: a common way to create a database, run migrations up and down 
    with a tool like [pressly/goose](https://github.com/pressly/goose), dump resulting schema and 
    compare to the schema checked in to git. Fail on diff.

3.  e2e tests, a common way to create a database from the checked in schema in git

You should already have a running postgres instance against which this package operates on.