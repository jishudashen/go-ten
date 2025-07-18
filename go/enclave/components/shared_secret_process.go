package components

import (
	"context"
	"fmt"

	"github.com/ten-protocol/go-ten/go/ethadapter/contractlib"

	gethcommon "github.com/ethereum/go-ethereum/common"
	gethcrypto "github.com/ethereum/go-ethereum/crypto"
	gethlog "github.com/ethereum/go-ethereum/log"
	"github.com/ten-protocol/go-ten/go/common"
	"github.com/ten-protocol/go-ten/go/common/log"
	"github.com/ten-protocol/go-ten/go/enclave/crypto"
	"github.com/ten-protocol/go-ten/go/enclave/storage"
)

type SharedSecretProcessor struct {
	enclaveRegistryLib  contractlib.EnclaveRegistryLib
	sharedSecretService *crypto.SharedSecretService
	attestationProvider AttestationProvider // interface for producing attestation reports and verifying them
	enclaveID           gethcommon.Address
	storage             storage.Storage
	logger              gethlog.Logger
	enclaveKeyService   *crypto.EnclaveAttestedKeyService
	L1ChainID           int64
}

func NewSharedSecretProcessor(enclaveRegistryLib contractlib.EnclaveRegistryLib, attestationProvider AttestationProvider, enclaveID gethcommon.Address, storage storage.Storage, sharedSecretService *crypto.SharedSecretService, logger gethlog.Logger, enclaveKeyService *crypto.EnclaveAttestedKeyService, L1ChainID int64) *SharedSecretProcessor {
	return &SharedSecretProcessor{
		enclaveRegistryLib:  enclaveRegistryLib,
		attestationProvider: attestationProvider,
		enclaveID:           enclaveID,
		storage:             storage,
		sharedSecretService: sharedSecretService,
		logger:              logger,
		enclaveKeyService:   enclaveKeyService,
		L1ChainID:           L1ChainID,
	}
}

// ProcessNetworkSecretMsgs we watch for all messages that are requesting or receiving the secret and we store the nodes attested keys
func (ssp *SharedSecretProcessor) ProcessNetworkSecretMsgs(ctx context.Context, processed *common.ProcessedL1Data, canShareSecret bool) []*common.ProducedSecretResponse {
	var responses []*common.ProducedSecretResponse
	block := processed.BlockHeader

	// process initialize secret events
	for _, txData := range processed.GetEvents(common.InitialiseSecretTx) {
		t, err := ssp.enclaveRegistryLib.DecodeTx(txData.Transaction)
		if err != nil {
			ssp.logger.Warn("Could not decode transaction", log.ErrKey, err)
			continue
		}
		initSecretTx, ok := t.(*common.L1InitializeSecretTx)
		if !ok {
			continue
		}

		att, err := common.DecodeAttestation(initSecretTx.Attestation)
		if err != nil {
			ssp.logger.Error("Could not decode attestation report", log.ErrKey, err)
			continue
		}

		if err := ssp.storeAttestation(ctx, att); err != nil {
			ssp.logger.Error("Could not store the attestation report.", log.ErrKey, err)
		}
	}

	// process secret requests
	for _, txData := range processed.GetEvents(common.SecretRequestTx) {
		t, err := ssp.enclaveRegistryLib.DecodeTx(txData.Transaction)
		if err != nil {
			ssp.logger.Warn("Could not decode transaction", log.ErrKey, err)
			continue
		}
		scrtReqTx, ok := t.(*common.L1RequestSecretTx)
		if !ok {
			continue
		}
		ssp.logger.Info("Process shared secret request.",
			log.BlockHeightKey, block,
			log.BlockHashKey, block.Hash(),
			log.TxKey, txData.Transaction.Hash())

		resp, err := ssp.processSecretRequest(ctx, scrtReqTx)
		if err != nil {
			ssp.logger.Error("Failed to process shared secret request.", log.ErrKey, err)
			continue
		}
		responses = append(responses, resp)
	}

	if !canShareSecret {
		return make([]*common.ProducedSecretResponse, 0)
	}

	return responses
}

func (ssp *SharedSecretProcessor) processSecretRequest(ctx context.Context, req *common.L1RequestSecretTx) (*common.ProducedSecretResponse, error) {
	att, err := common.DecodeAttestation(req.Attestation)
	if err != nil {
		return nil, fmt.Errorf("failed to decode attestation - %w", err)
	}

	ssp.logger.Info("received attestation", "attestation", att)
	secret, err := ssp.verifyAttestationAndEncryptSecret(ctx, att)
	if err != nil {
		return nil, fmt.Errorf("secret request failed, no response will be published - %w", err)
	}

	// Store the attested key only if the attestation process succeeded.
	err = ssp.storeAttestation(ctx, att)
	if err != nil {
		return nil, fmt.Errorf("could not store attestation, no response will be published. Cause: %w", err)
	}

	// Create the hash that needs to be signed for the network secret response
	enclaveRegistryAddress := ssp.enclaveRegistryLib.GetContractAddr()
	hash, err := crypto.CreateNetworkSecretResponseHash(
		att.EnclaveID,
		secret,
		ssp.L1ChainID, // L1 network chain ID
		*enclaveRegistryAddress,
	)
	if err != nil {
		return nil, fmt.Errorf("failed to create network secret response hash: %w", err)
	}

	// Sign the hash
	signature, err := ssp.enclaveKeyService.Sign(hash)
	if err != nil {
		return nil, fmt.Errorf("failed to sign network secret response: %w", err)
	}

	ssp.logger.Trace("Processed secret request.", "owner", att.EnclaveID)
	// todo (@matt) - we need to make sure that the attestation report is signed by the enclave's private key
	return &common.ProducedSecretResponse{
		Secret:      secret,
		RequesterID: att.EnclaveID,
		AttesterID:  ssp.enclaveID,
		HostAddress: att.HostAddress,
		Signature:   signature,
	}, nil
}

// ShareSecret verifies the request and if it trusts the report and the public key it will return the secret encrypted with that public key.
func (ssp *SharedSecretProcessor) verifyAttestationAndEncryptSecret(_ context.Context, att *common.AttestationReport) (common.EncryptedSharedEnclaveSecret, error) {
	// First we verify the attestation report has come from a valid obscuro enclave running in a verified TEE.
	data, err := ssp.attestationProvider.VerifyReport(att)
	if err != nil {
		return nil, fmt.Errorf("unable to verify report - %w", err)
	}
	// Then we verify the public key provided has come from the same enclave as that attestation report
	if err = VerifyIdentity(data, att); err != nil {
		return nil, fmt.Errorf("unable to verify identity - %w", err)
	}
	ssp.logger.Info(fmt.Sprintf("Successfully verified attestation and identity. Owner: %s", att.EnclaveID))

	return ssp.sharedSecretService.EncryptSecretWithKey(att.PubKey)
}

// storeAttestation stores the attested keys of other nodes so we can decrypt their rollups
func (ssp *SharedSecretProcessor) storeAttestation(ctx context.Context, att *common.AttestationReport) error {
	ssp.logger.Info(fmt.Sprintf("Store attestation. Owner: %s", att.EnclaveID))
	// Store the attestation
	key, err := gethcrypto.DecompressPubkey(att.PubKey)
	if err != nil {
		return fmt.Errorf("failed to parse public key %w", err)
	}
	err = ssp.storage.StoreNewEnclave(ctx, att.EnclaveID, key)
	if err != nil {
		return fmt.Errorf("could not store attested key. Cause: %w", err)
	}
	return nil
}
