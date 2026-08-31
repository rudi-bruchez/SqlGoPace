\# Global change : implement maintenance operations



\## Goal



There are maintenance operations we want to Schedule on the server, mostly :



* DBCC CHECKDB
* update statistics
* reorganize / rebuild indexes



The real danger here is "reorganize / rebuild indexes", that can block sessions or fill in the transaction log.



SqlGoSpace has already the insfrastructure to run and monitor these maintenance operations. But it's geared towards predefined discret operations, here it should generate DDL commands from the database contents



And I would add an operation I'd love SqlGoPace to handle : compress all object, using a predefined rule, for instance, test compression in ROW or PAGE on objects, and choose PAGE only is the gain is substantial.

Or prevent some tables to be compressed in PAGE, because we know they are write intensive (and SqlGoPace could check that using sys.dm\_db\_index\_operational\_stats() and have some rules to chose by itself)

And : it could decide partition by partition, detecting alive partitions where ROW compression should be better

And : NONE compression if there is no real gain



\## Existing Tools



Ola Hallengren scripts are widely used.

https://ola.hallengren.com/sql-server-index-and-statistics-maintenance.html



But the don't monitor the execution like SqlGoPace do, and they don't dynamically choose RESUMABLE, etc.



\## PLAN MODE FIRST



Think about the spec here, and analyze needs, good ideas, possibilities, dangers, best implementations, and ask questions if needed. Then write a first spec file in the @docs/specs/ folder. 



Some questions are :

* how to keep the current functionalities and allow this new running mode?
* how to analyze objects and keep the list of objects to be processed / to process ?
* should we store that in an SQL table for history?
* on some server, REBUILD should be banned for big tables, only REORGANIZE, how to choose and coe that? In config? Automatically? Automtic seems Dangerous and random. Or we could have both ways

