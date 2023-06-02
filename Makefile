test:
	go test ./...

vectors: test
	ED25519_BLINDING_TEST_VECTORS_OUT=type3-ed25519-blinding-test-vectors.json go test -v -run TestVectorGenerateEd25519Blinding ./... 
	ECDSA_BLINDING_TEST_VECTORS_OUT=type3-ecdsa-blinding-test-vectors.json go test -v -run TestVectorGenerateECDSABlinding ./... 
	TOKEN_TEST_VECTORS_OUT=token-test-vectors.json go test -v -run TestVectorGenerateToken ./... 
	TYPE1_ISSUANCE_TEST_VECTORS_OUT=type1-issuance-test-vectors.json go test -v -run TestVectorGenerateBasicPrivateIssuance ./... 
	TYPEF91A_ISSUANCE_TEST_VECTORS_OUT=typeF91A-issuance-test-vectors.json go test -v -run TestVectorGenerateBatchedPrivateIssuance ./... 
	TYPE2_ISSUANCE_TEST_VECTORS_OUT=type2-issuance-test-vectors.json go test -v -run TestVectorGenerateBasicIssuance ./... 
	TYPE3_ANON_ORIGIN_ID_TEST_VECTORS_OUT=type3-anon-origin-id-test-vectors.json go test -v -run TestVectorGenerateAnonOriginID ./... 
	TYPE3_ORIGIN_ENCRYPTION_TEST_VECTORS_OUT=type3-origin-encryption-test-vectors.json go test -v -run TestVectorGenerateOriginEncryption ./... 

bench:
	go test -bench=.