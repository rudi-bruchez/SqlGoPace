USE [master]
GO
CREATE LOGIN [sqlgopace] WITH PASSWORD=N'xxx', 
    DEFAULT_DATABASE=[master], CHECK_EXPIRATION=OFF, CHECK_POLICY=OFF
GO
GRANT VIEW SERVER STATE TO [sqlgopace];
GO
USE [mydb]
GO
CREATE USER [sqlgopace] FOR LOGIN [sqlgopace]
ALTER ROLE [db_datareader] ADD MEMBER [sqlgopace]
ALTER ROLE [db_ddladmin] ADD MEMBER [sqlgopace]
GO
